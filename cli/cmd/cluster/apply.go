// Copyright 2020 The Lokomotive Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cluster

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"
	watchtools "k8s.io/client-go/tools/watch"
	"sigs.k8s.io/yaml"

	"github.com/kinvolk/lokomotive/internal"
	"github.com/kinvolk/lokomotive/pkg/k8sutil"
	"github.com/kinvolk/lokomotive/pkg/lokomotive"
	"github.com/kinvolk/lokomotive/pkg/platform"
	"github.com/kinvolk/lokomotive/pkg/terraform"
)

// ApplyOptions defines how cluster apply operation will behave.
type ApplyOptions struct {
	Confirm                  bool
	UpgradeKubelets          bool
	SkipComponents           bool
	SkipPreUpdateHealthCheck bool
	Verbose                  bool
	ConfigPath               string
	ValuesPath               string
}

// Apply applies cluster configuration together with components.
//
//nolint:funlen
func Apply(contextLogger *log.Entry, options ApplyOptions) error {
	cc := clusterConfig{
		verbose:    options.Verbose,
		configPath: options.ConfigPath,
		valuesPath: options.ValuesPath,
	}

	c, err := cc.initialize(contextLogger)
	if err != nil {
		return fmt.Errorf("initializing: %w", err)
	}

	exists, err := clusterExists(c.terraformExecutor)
	if err != nil {
		return fmt.Errorf("checking if cluster exists: %w", err)
	}

	// Prepare for getting kubeconfig.
	kg := kubeconfigGetter{
		platformRequired: true,
		clusterConfig:    cc,
	}

	var kubeconfig []byte

	// Prepare controlplane updater.
	cu := controlplaneUpdater{
		kubeconfig:    kubeconfig,
		assetDir:      c.assetDir,
		contextLogger: *contextLogger,
		ex:            c.terraformExecutor,
	}

	charts := platform.CommonControlPlaneCharts(options.UpgradeKubelets)

	if !exists {
		if err := c.unpackControlplaneCharts(); err != nil {
			return fmt.Errorf("unpacking controlplane assets: %w", err)
		}
	}

	if exists && !options.Confirm {
		// TODO: We could plan to a file and use it when installing.
		if err := c.terraformExecutor.Plan(); err != nil {
			return fmt.Errorf("reconciling cluster state: %v", err)
		}

		if !askForConfirmation("Do you want to proceed with cluster apply?") {
			contextLogger.Println("Cluster apply cancelled")

			return nil
		}
	}

	if exists && !options.SkipPreUpdateHealthCheck {
		var err error

		cu.kubeconfig, err = kg.getKubeconfig(contextLogger, c.lokomotiveConfig)
		if err != nil {
			return fmt.Errorf("getting kubeconfig: %v", err)
		}

		for _, c := range charts {
			if err := cu.ensureComponent(c.Name, c.Namespace); err != nil {
				return fmt.Errorf("ensuring controlplane component %q: %w", c.Name, err)
			}
		}
	}

	f := func(resourceName string) string {
		m := c.platform.Meta()
		return fmt.Sprintf("module.%s-%s.module.bootkube.%s", m.Name, m.ClusterName, resourceName)
	}

	// If we are not on managed platform, rotate all certificates while upgrading.
	if exists && !c.platform.Meta().Managed {
		steps := []terraform.ExecutionStep{}
		targets := []string{
			"tls_locally_signed_cert.admin",
			"tls_locally_signed_cert.admission-webhook-server",
			"tls_locally_signed_cert.aggregation-client[0]",
			"tls_locally_signed_cert.apiserver",
			"tls_locally_signed_cert.client",
			"tls_locally_signed_cert.kubelet",
			"tls_locally_signed_cert.peer",
			"tls_locally_signed_cert.server",
			"tls_self_signed_cert.aggregation-ca[0]",
			"tls_self_signed_cert.etcd-ca",
			"tls_self_signed_cert.kube-ca",
		}

		for _, t := range targets {
			steps = append(steps, terraform.ExecutionStep{
				Args: []string{"taint", f(t)},
			})
		}

		if err := c.terraformExecutor.Execute(steps...); err != nil {
			return fmt.Errorf("tainting existing certificates: %w", err)
		}
	}

	if err := c.platform.Apply(&c.terraformExecutor); err != nil {
		return fmt.Errorf("applying platform: %v", err)
	}

	fmt.Printf("\nYour configurations are stored in %s\n", c.assetDir)

	kubeconfig, err = kg.getKubeconfig(contextLogger, c.lokomotiveConfig)
	if err != nil {
		return fmt.Errorf("getting kubeconfig: %v", err)
	}

	// Update all the pre installed namespaces with lokomotive specific label.
	// `lokomotive.kinvolk.io/name: <namespace_name>`.
	if err := updateInstalledNamespaces(kubeconfig); err != nil {
		return fmt.Errorf("updating installed namespace: %v", err)
	}

	// Do controlplane upgrades only if cluster already exists and it is not a managed platform.
	if exists && !c.platform.Meta().Managed {
		fmt.Printf("\nEnsuring that cluster controlplane is up to date.\n")

		cu := controlplaneUpdater{
			kubeconfig:    kubeconfig,
			assetDir:      c.assetDir,
			contextLogger: *contextLogger,
			ex:            c.terraformExecutor,
		}

		if err := c.unpackControlplaneCharts(); err != nil {
			return fmt.Errorf("unpacking controlplane assets: %w", err)
		}

		for _, c := range charts {
			if err := cu.upgradeComponent(c.Name, c.Namespace); err != nil {
				return fmt.Errorf("upgrading controlplane component %q: %w", c.Name, err)
			}
		}

		valuesRaw := ""

		if err := c.terraformExecutor.Output("kubernetes_values", &valuesRaw); err != nil {
			return fmt.Errorf("getting %q release values from Terraform output: %w", "kubernetes", err)
		}

		values := &kubernetesValues{}
		if err := yaml.Unmarshal([]byte(valuesRaw), values); err != nil {
			return fmt.Errorf("parsing kubeconfig values: %w", err)
		}

		cs, err := k8sutil.NewClientset(kubeconfig)
		if err != nil {
			return fmt.Errorf("creating clientset from kubeconfig: %w", err)
		}

		caCert, err := base64.StdEncoding.DecodeString(values.ControllerManager.CACert)
		if err != nil {
			return fmt.Errorf("base64 decode: %w", err)
		}

		for {
			secrets, err := cs.CoreV1().Secrets("").List(context.TODO(), metav1.ListOptions{
				FieldSelector: "type=kubernetes.io/service-account-token",
			})
			if err != nil {
				return fmt.Errorf("getting secrets: %v", err)
			}

			allUpToDate := true

			for _, v := range secrets.Items {

				if string(v.Data["ca.crt"]) != string(caCert) {
					contextLogger.Printf("%s/%s is not up to date yet", v.Namespace, v.Name)

					allUpToDate = false
				}
			}

			if allUpToDate {
				contextLogger.Printf("all service account tokens are up to date and have new CA certificate")

				break
			}
		}

		// Restart all system DaemonSets which use service account tokens, as they
		// load Kubernetes CA certificate from it.
		daemonSets := []string{
			"calico-node",
			"coredns",
			"kube-controller-manager",
			"kube-proxy",
			"kube-scheduler",
			"pod-checkpointer",
			// Kubelet does not use service account token, but takes CA certificate from it and writes it to the disk,
			// so it should be restarted as well.
			"kubelet",
		}

		l := contextLogger
		ns := "kube-system"

		for _, name := range daemonSets {
			l.Printf("rolling restart of %s/%s", ns, name)

			ds, err := cs.AppsV1().DaemonSets(ns).Get(context.TODO(), name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("getting DaemonSet: %w", err)
			}

			if ds.Spec.Template.Annotations == nil {
				ds.Spec.Template.Annotations = map[string]string{}
			}

			ds.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().String()

			newDS, err := cs.AppsV1().DaemonSets(ns).Update(context.TODO(), ds, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("updating DaemonSet: %w", err)
			}

			fieldSelector := fields.OneTermEqualSelector("metadata.name", name).String()
			lw := &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.FieldSelector = fieldSelector
					return cs.AppsV1().DaemonSets(ns).List(context.TODO(), options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.FieldSelector = fieldSelector
					return cs.AppsV1().DaemonSets(ns).Watch(context.TODO(), options)
				},
			}

			l.Printf("Updated DS status %+v", newDS.Status)

			time.Sleep(10 * time.Second)

			// if the rollout isn't done yet, keep watching deployment status
			//ctx, cancel := watchtools.ContextWithOptionalTimeout(context.Background(), 10*time.Minute)
			_, err = watchtools.UntilWithSync(context.TODO(), lw, &appsv1.DaemonSet{}, nil, func(e watch.Event) (bool, error) {
				switch t := e.Type; t {
				case watch.Added, watch.Modified:
					ds := e.Object.(*appsv1.DaemonSet)

					/*if revision != "" {
						if revision != ds.Annotations["deployment.kubernetes.io/revision"] {
							return false, fmt.Errorf("desired revision (%d) is different from the running revision (%d)", revision, ds.Annotations["deployment.kubernetes.io/revision"])
						}
					}

					done := false

					if ds.Generation <= ds.Status.ObservedGeneration {
						done = func() bool {
							var cond *appsv1.DeploymentCondition
							for i := range ds.Status.Conditions {
								c := ds.Status.Conditions[i]
								if c.Type == appsv1.DeploymentProgressing {
									cond = &c
								}
							}
							if cond != nil && cond.Reason == ProgressDeadlineExceeded {
								return false
							}
							if ds.Spec.Replicas != nil && ds.Status.UpdatedReplicas < *ds.Spec.Replicas {
								return false
							}
							if deployment.Status.Replicas > deployment.Status.UpdatedReplicas {
								return false
							}
							if deployment.Status.AvailableReplicas < deployment.Status.UpdatedReplicas {
								return false
							}

							return true
						}()
					}*/

					done, err := func() (bool, error) {
						if ds.Spec.UpdateStrategy.Type != appsv1.RollingUpdateDaemonSetStrategyType {
							return true, fmt.Errorf("rollout status is only available for %s strategy type", appsv1.RollingUpdateStatefulSetStrategyType)
						}

						l.Printf("received DS status %+v", ds.Status)

						if ds.Status.ObservedGeneration < newDS.Generation {
							l.Printf("daemonset not updaated yet %s", name)

							return false, nil
						}
						replicas := ds.Status.DesiredNumberScheduled

						if replicas == 0 {
							l.Printf("no replicas scheduled for daemonset %s", name)

							return false, nil
						}

						l.Printf("daemonset: %s/%s, expected: %d desired: %d ready: %d updated: %d",
							ns, name, replicas, ds.Status.DesiredNumberScheduled, ds.Status.NumberReady, ds.Status.UpdatedNumberScheduled)

						if ds.Status.NumberReady == replicas && ds.Status.UpdatedNumberScheduled == replicas {
							l.Println("found required replicas")

							return true, nil
						}
						return false, nil
						/*
							if ds.Generation <= ds.Status.ObservedGeneration && ds.Generation <= newDS.Generation {
								if ds.Status.UpdatedNumberScheduled < ds.Status.DesiredNumberScheduled {
									l.Printf("Waiting for daemon set %q rollout to finish: %d out of %d new pods have been updated...\n", ds.Name, ds.Status.UpdatedNumberScheduled, ds.Status.DesiredNumberScheduled)
									return false, nil
								}
								if ds.Status.NumberAvailable < ds.Status.DesiredNumberScheduled {
									l.Printf("Waiting for daemon set %q rollout to finish: %d of %d updated pods are available...\n", ds.Name, ds.Status.NumberAvailable, ds.Status.DesiredNumberScheduled)
									return false, nil
								}
							}*/

						//l.Printf("daemonset ready")

						//return true, nil
					}()

					if err != nil {
						return false, fmt.Errorf("checking daemonset status: %w", err)
					}

					// Quit waiting if the rollout is done
					if done {
						return true, nil
					}

					return false, nil
				case watch.Deleted:
					// We need to abort to avoid cases of recreation and not to silently watch the wrong (new) object
					return true, fmt.Errorf("object has been deleted")

				default:
					return true, fmt.Errorf("internal error: unexpected event %#v", e)
				}
			})

			if err != nil {
				return fmt.Errorf("waiting for daemonset to restart: %w", err)
			}
		}

		// Restart all system Deployments which use service account tokens, as they
		// load Kubernetes CA certificate from it.
		deploys := []struct {
			name      string
			namespace string
		}{
			{
				name:      "calico-kube-controllers",
				namespace: "kube-system",
			},
			{
				name:      "admission-webhook-server",
				namespace: "lokomotive-system",
			},
		}

		for _, deploy := range deploys {
			l.Printf("rolling restart of %s/%s", deploy.name, deploy.namespace)

			dc := cs.AppsV1().Deployments(deploy.namespace)
			d, err := dc.Get(context.TODO(), deploy.name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("getting DaemonSet: %w", err)
			}

			if d.Spec.Template.Annotations == nil {
				d.Spec.Template.Annotations = map[string]string{}
			}

			d.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().String()

			newD, err := dc.Update(context.TODO(), d, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("updating Deployment: %w", err)
			}

			fieldSelector := fields.OneTermEqualSelector("metadata.name", deploy.name).String()
			lw := &cache.ListWatch{
				ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
					options.FieldSelector = fieldSelector
					return dc.List(context.TODO(), options)
				},
				WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
					options.FieldSelector = fieldSelector
					return dc.Watch(context.TODO(), options)
				},
			}

			l.Printf("Updated Deployment status %+v", newD.Status)

			// Wait a bit to make sure kube-controller-manager picks up the update.
			// TODO: make this determinstic.
			time.Sleep(10 * time.Second)

			// if the rollout isn't done yet, keep watching deployment status
			//ctx, cancel := watchtools.ContextWithOptionalTimeout(context.Background(), 10*time.Minute)
			_, err = watchtools.UntilWithSync(context.TODO(), lw, &appsv1.Deployment{}, nil, func(e watch.Event) (bool, error) {
				switch t := e.Type; t {
				case watch.Added, watch.Modified:
					d := e.Object.(*appsv1.Deployment)

					done, err := func() (bool, error) {
						if newD.Generation <= d.Status.ObservedGeneration {
							var cond *appsv1.DeploymentCondition

							for i := range d.Status.Conditions {
								c := d.Status.Conditions[i]
								if c.Type == appsv1.DeploymentProgressing {
									cond = &c
								}
							}

							if cond != nil && cond.Reason == "ProgressDeadlineExceeded" {
								return false, fmt.Errorf("deployment %q exceeded its progress deadline", d.Name)
							}

							if d.Spec.Replicas != nil && d.Status.UpdatedReplicas < *d.Spec.Replicas {
								l.Printf("Waiting for deployment %q rollout to finish: %d out of %d new replicas have been updated...\n", d.Name, d.Status.UpdatedReplicas, *d.Spec.Replicas)

								return false, nil
							}

							if d.Status.Replicas > d.Status.UpdatedReplicas {
								l.Printf("Waiting for deployment %q rollout to finish: %d old replicas are pending termination...\n", d.Name, d.Status.Replicas-d.Status.UpdatedReplicas)

								return false, nil
							}

							if d.Status.AvailableReplicas < d.Status.UpdatedReplicas {
								l.Printf("Waiting for deployment %q rollout to finish: %d of %d updated replicas are available...\n", d.Name, d.Status.AvailableReplicas, d.Status.UpdatedReplicas)

								return false, nil
							}

							l.Printf("deployment %q successfully rolled out\n", d.Name)

							return true, nil
						}

						l.Printf("Waiting for deployment spec update to be observed...\n")

						return false, nil
					}()

					if err != nil {
						return false, fmt.Errorf("checking daemonset status: %w", err)
					}

					// Quit waiting if the rollout is done
					if done {
						return true, nil
					}

					return false, nil
				case watch.Deleted:
					// We need to abort to avoid cases of recreation and not to silently watch the wrong (new) object
					return true, fmt.Errorf("object has been deleted")

				default:
					return true, fmt.Errorf("internal error: unexpected event %#v", e)
				}
			})

			if err != nil {
				return fmt.Errorf("waiting for daemonset to restart: %w", err)
			}
		}
	}

	if ph, ok := c.platform.(platform.PlatformWithPostApplyHook); ok {
		if err := ph.PostApplyHook(kubeconfig); err != nil {
			return fmt.Errorf("running platform post install hook: %v", err)
		}
	}

	if err := verifyCluster(kubeconfig, c.platform.Meta().ExpectedNodes); err != nil {
		return fmt.Errorf("verifying cluster: %v", err)
	}

	if options.SkipComponents {
		return nil
	}

	componentObjects, err := componentNamesToObjects(selectComponentNames(nil, *c.lokomotiveConfig.RootConfig))
	if err != nil {
		return fmt.Errorf("getting component objects: %w", err)
	}

	contextLogger.Println("Applying component configuration")

	if err := applyComponents(c.lokomotiveConfig, kubeconfig, componentObjects); err != nil {
		return fmt.Errorf("applying component configuration: %v", err)
	}

	return nil
}

type controllerManagerValues struct {
	CACert string `json:"caCert"`
}

type kubernetesValues struct {
	ControllerManager controllerManagerValues `json:"controllerManager"`
}

func verifyCluster(kubeconfig []byte, expectedNodes int) error {
	cs, err := k8sutil.NewClientset(kubeconfig)
	if err != nil {
		return fmt.Errorf("creating Kubernetes clientset: %w", err)
	}

	cluster := lokomotive.NewCluster(cs, expectedNodes)

	return cluster.Verify()
}

func updateInstalledNamespaces(kubeconfig []byte) error {
	cs, err := k8sutil.NewClientset(kubeconfig)
	if err != nil {
		return fmt.Errorf("create clientset: %v", err)
	}

	nsclient := cs.CoreV1().Namespaces()

	namespaces, err := k8sutil.ListNamespaces(nsclient)
	if err != nil {
		return fmt.Errorf("getting list of namespaces: %v", err)
	}

	for _, ns := range namespaces.Items {
		ns := k8sutil.Namespace{
			Name: ns.ObjectMeta.Name,
			Labels: map[string]string{
				internal.NamespaceLabelKey: ns.ObjectMeta.Name,
			},
		}

		if err := k8sutil.CreateOrUpdateNamespace(ns, nsclient); err != nil {
			return fmt.Errorf("namespace %q with labels: %v", ns, err)
		}
	}

	return nil
}
