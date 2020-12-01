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

// +build aws aws_edge baremetal packet
// +build e2e

package system_test

import (
	"context"
	"testing"
	"time"

	testutil "github.com/kinvolk/lokomotive/test/components/util"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// Define manifest as YAML and then unmarshal it to a Go struct so that it is easier to
// write and debug, as it can be copy-pasted to a file and applied manually.
const dockerLiveRestoreOnNodesDSManifest = `apiVersion: apps/v1
kind: DaemonSet
metadata:
  generateName: test-live-restore-
spec:
  selector:
    matchLabels:
      name: test-live-restore
  template:
    metadata:
      labels:
        name: test-live-restore
    spec:
      tolerations:
      - key: node-role.kubernetes.io/master
        effect: NoSchedule
      terminationGracePeriodSeconds: 1
      containers:
      - name: test-live-restore
        image: quay.io/kinvolk/docker:stable
        imagePullPolicy: IfNotPresent
        command:
        - /bin/sh
        args:
        - -c
        - 'docker info -f {{.LiveRestoreEnabled}} | grep true && sleep infinity'
        volumeMounts:
        - name: docker-socket
          mountPath: /var/run/docker.sock
      volumes:
      - name: docker-socket
        hostPath:
          path: /var/run/docker.sock
`

// TestDockerLiveRestoreOnNodes runs `docker info -f {{.LiveRestoreEnabled}} | grep true && sleep
// infinity` inside the container. If it returns `true` then the pods will sleep. If not it will go
// into CrashLoopBackOff and test will fail.
func TestDockerLiveRestoreOnNodes(t *testing.T) {
	t.Parallel()

	namespace := "kube-system"
	client := testutil.CreateKubeClient(t)

	ds := &appsv1.DaemonSet{}
	if err := yaml.Unmarshal([]byte(dockerLiveRestoreOnNodesDSManifest), ds); err != nil {
		t.Fatalf("failed unmarshaling manifest: %v", err)
	}

	ds, err := client.AppsV1().DaemonSets(namespace).Create(context.TODO(), ds, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create DaemonSet: %v", err)
	}

	// It takes some time for a pod to go into `CrashLoopBackOff`. This sleep helps against false
	// positives, a pod which is marked as `Running` for some time isn't mistakenly counted.
	t.Logf("Sleeping for a minute...")
	time.Sleep(time.Minute)

	testutil.WaitForDaemonSet(t, client, namespace, ds.ObjectMeta.Name, time.Second*5, time.Minute*5)

	t.Cleanup(func() {
		if err := client.AppsV1().DaemonSets(namespace).Delete(
			context.TODO(), ds.ObjectMeta.Name, metav1.DeleteOptions{}); err != nil {
			t.Logf("failed to remove DaemonSet: %v", err)
		}
	})
}
