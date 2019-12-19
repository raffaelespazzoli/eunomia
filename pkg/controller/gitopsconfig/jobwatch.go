/*
Copyright 2019 Kohl's Department Stores, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gitopsconfig

import (
	"context"

	"golang.org/x/xerrors"
	batchv1 "k8s.io/api/batch/v1"
	batchv1beta1 "k8s.io/api/batch/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gitopsv1alpha1 "github.com/KohlsTechnology/eunomia/pkg/apis/eunomia/v1alpha1"
)

// addJobWatch starts watching Job events in the Kubernetes cluster as
// specified by kubecfg. The handler will be called for every Job event
// detected. The returned func should be called to stop the watch and free
// associated resources.
func addJobWatch(kubecfg *rest.Config, handler cache.ResourceEventHandler) (func(), error) {
	// based on: http://web.archive.org/web/20161221032701/https://solinea.com/blog/tapping-kubernetes-events
	clientset, err := kubernetes.NewForConfig(kubecfg)
	if err != nil {
		return nil, xerrors.Errorf("cannot create Job watcher from controller manager: %w", err)
	}
	watchlist := cache.NewListWatchFromClient(clientset.Batch().RESTClient(), "jobs", corev1.NamespaceAll, fields.Everything())
	// https://stackoverflow.com/a/49231503/98528
	// TODO: what is the difference vs. NewSharedInformer?
	_, controller := cache.NewInformer(watchlist, &batchv1.Job{}, 0, handler)

	stopChan := make(chan struct{})
	go controller.Run(stopChan)
	return func() { close(stopChan) }, nil
}

// jobCompletionEmitter emits Job completion events ("JobSuccessful",
// "JobFailed") into cluster when it is used as a ResourceEventHandler on
// batchv1.Job objects. For more details, see OnUpdate method. The OnAdd and
// OnDelete methods pass their argument to OnUpdate.
type jobCompletionEmitter struct {
	client        client.Client
	eventRecorder record.EventRecorder
}

var _ cache.ResourceEventHandler = &jobCompletionEmitter{}

func (e *jobCompletionEmitter) OnAdd(newObj interface{})    { e.OnUpdate(nil, newObj) }
func (e *jobCompletionEmitter) OnDelete(oldObj interface{}) { e.OnUpdate(oldObj, nil) }

// OnUpdate detects if newObj is a completed Job while oldObj is not, and emits
// a Job completion event into the cluster in such case. It requires that the
// arguments are either *batchv1.Job objects or nil.
//
// For JobSuccessful to be emitted, newJob must:
//  - be owned by GitOpsConfig, directly or through a CronJob,
//  - have .Status.Active == 0,
//  - have .Status.Succeeded > 0.
//
// For JobFailed to be emitted, conditions are similar as for JobSuccessful,
// except for the last one being:
//  - have .Status.Succeeded == 0 and .Status.Failed > 0.
func (e *jobCompletionEmitter) OnUpdate(oldObj, newObj interface{}) {
	// Extract Job objects from arguments
	oldJob, ok := oldObj.(*batchv1.Job)
	if !ok && oldObj != nil {
		log.Error(nil, "non-Job object passed to jobCompletionEmitter", "oldObj", oldObj, "newObj", newObj)
		return
	}
	newJob, ok := newObj.(*batchv1.Job)
	if !ok && newObj != nil {
		log.Error(nil, "non-Job object passed to jobCompletionEmitter", "oldObj", oldObj, "newObj", newObj)
		return
	}

	// Check some preconditions that can let us quickly ignore the Job change.
	switch {
	case newJob == nil:
		return // Job deletion event - no need to check for completion.
	case newJob.Status.Active > 0:
		return // The Job is not completed yet, don't emit any events.
	case oldJob != nil &&
		oldJob.Status.Active == 0 &&
		oldJob.Status.Succeeded+oldJob.Status.Failed >= 1:
		// TODO: write a unit test verifying we enter this case, with some real data received in OnDelete
		return // The Job was already completed before
	}

	// Check if this is a Job that's owned by GitOpsConfig.
	gitopsRef, err := e.findJobOwner(newJob)
	if err != nil {
		log.Error(err, "cannot find Job's owner")
		return
	}
	if gitopsRef == nil {
		// Got an event for a job not owned by GitOpsConfig - ignore it.
		return
	}
	gitops := &gitopsv1alpha1.GitOpsConfig{
		TypeMeta: metav1.TypeMeta{
			Kind:       gitopsRef.Kind,
			APIVersion: gitopsRef.APIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: gitopsRef.Name,
			// Note: Assuming the same namespace for GitOpsConfig as for the Job
			Namespace: newJob.GetNamespace(),
		},
	}

	// Emit an event with detailed contents
	annotation := map[string]string{
		"job": newJob.GetName(),
	}
	status := newJob.Status
	switch {
	case status.Succeeded == 1:
		// Some Pods may have failed initially because of intermittent issues,
		// but eventually one succeeded, so the Job is deemed successful.
		e.eventRecorder.AnnotatedEventf(gitops, annotation, "Normal", "JobSuccessful",
			"Job finished successfully: %s", newJob.GetName())
	case status.Succeeded == 0 && status.Failed > 0:
		e.eventRecorder.AnnotatedEventf(gitops, annotation, "Warning", "JobFailed",
			"Job failed: %s", newJob.GetName())
	}
}

// findJobOwner checks if the job is owned by a GitOpsConfig (either directly,
// or through a CronJob intermediary). If yes, it returns a reference to the
// GitOpsConfig. If not, it returns nil.
//
// TODO: it would be nice if we could generalize it to just walk the tree of
// owners (possibly only where Controller==true), and search if any one of them
// is a GitOpsConfig - instead of having to special-case a CronJob as a
// possible intermediary.
func (e *jobCompletionEmitter) findJobOwner(job *batchv1.Job) (*metav1.OwnerReference, error) {
	const gitopsKind = "GitOpsConfig"

	// Is the job owned directly by GitOpsConfig?
	gitopsRef := getOwnerByKind(job, gitopsKind)
	if gitopsRef != nil {
		return gitopsRef, nil
	}

	// Is the job owned by a CronJob, which is then owned by GitOpsConfig?
	cronjobRef := getOwnerByKind(job, "CronJob")
	if cronjobRef == nil {
		return nil, nil
	}
	cronjob := &batchv1beta1.CronJob{}
	err := e.client.Get(context.TODO(),
		types.NamespacedName{Name: cronjobRef.Name, Namespace: job.GetNamespace()},
		cronjob)
	if err != nil {
		return nil, xerrors.Errorf("cannot load CronJob owner %q of Job %q: %w", cronjobRef.Name, job.GetName(), err)
	}
	gitopsRef = getOwnerByKind(cronjob, gitopsKind)
	return gitopsRef, nil
}

// getOwnerByKind searches the direct owners of obj. It returns a reference to
// an owner of the specified kind, or nil if a matching one was not found.
func getOwnerByKind(obj metav1.Object, kind string) *metav1.OwnerReference {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == kind {
			return &ref
		}
	}
	return nil
}
