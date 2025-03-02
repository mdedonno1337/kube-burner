// Copyright 2020 The Kube-burner Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package burner

import (
	"context"
	"time"

	"github.com/cloud-bulldozer/kube-burner/log"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

func createNamespace(clientset *kubernetes.Clientset, namespaceName string, nsLabels map[string]string) error {
	ns := v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespaceName, Labels: nsLabels},
	}

	return RetryWithExponentialBackOff(func() (done bool, err error) {
		_, err = clientset.CoreV1().Namespaces().Create(context.TODO(), &ns, metav1.CreateOptions{})
		if errors.IsForbidden(err) {
			log.Fatalf("authorization error creating namespace %s: %s", ns.Name, err)
			return false, err
		}
		if errors.IsAlreadyExists(err) {
			log.Infof("Namespace %s already exists", ns.Name)
			nsSpec, _ := clientset.CoreV1().Namespaces().Get(context.TODO(), namespaceName, metav1.GetOptions{})
			if nsSpec.Status.Phase == v1.NamespaceTerminating {
				log.Warnf("Namespace %s is in %v state, retrying", namespaceName, v1.NamespaceTerminating)
				return false, nil
			}
			return true, nil
		} else if err != nil {
			log.Errorf("unexpected error creating namespace %s: %v", namespaceName, err)
			return false, nil
		}
		log.Debugf("Created namespace: %s", ns.Name)
		return true, err
	}, 5*time.Second, 3, 0, 8)
}

// CleanupNamespaces deletes namespaces with the given selector
func CleanupNamespaces(ctx context.Context, l metav1.ListOptions) {
	ns, _ := ClientSet.CoreV1().Namespaces().List(ctx, l)
	if len(ns.Items) > 0 {
		log.Infof("Deleting namespaces with label %s", l.LabelSelector)
		for _, ns := range ns.Items {
			err := ClientSet.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{})
			if errors.IsNotFound(err) {
				log.Warnf("Namespace %s not found", ns.Name)
				continue
			}
			if ctx.Err() == context.DeadlineExceeded {
				log.Fatalf("Timeout cleaning up namespaces: %v", err)
			}
			if err != nil {
				log.Errorf("Error cleaning up namespaces: %v", err)
			}
		}
	}
	if len(ns.Items) > 0 {
		if err := waitForDeleteNamespaces(ctx, l); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				log.Fatalf("Timeout cleaning up namespaces: %v", err)
			}
			if err != nil {
				log.Errorf("Error cleaning up namespaces: %v", err)
			}
		}
	}
}

func waitForDeleteNamespaces(ctx context.Context, l metav1.ListOptions) error {
	log.Info("Waiting for namespaces to be definitely deleted")
	err := wait.PollImmediateUntilWithContext(ctx, time.Second, func(ctx context.Context) (bool, error) {
		ns, err := ClientSet.CoreV1().Namespaces().List(ctx, l)
		if err != nil {
			return false, err
		}
		if len(ns.Items) == 0 {
			return true, nil
		}
		log.Debugf("Waiting for %d namespaces labeled with %s to be deleted", len(ns.Items), l.LabelSelector)
		return false, nil
	})
	return err
}
