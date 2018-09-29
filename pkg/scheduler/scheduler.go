/*
Copyright 2014 The Kubernetes Authors.

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

package scheduler

import (
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	policyinformers "k8s.io/client-go/informers/policy/v1beta1"
	storageinformers "k8s.io/client-go/informers/storage/v1"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/algorithm"
	"k8s.io/kubernetes/pkg/scheduler/algorithm/predicates"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/api"
	schedulercache "k8s.io/kubernetes/pkg/scheduler/cache"
	"k8s.io/kubernetes/pkg/scheduler/core"
	"k8s.io/kubernetes/pkg/scheduler/core/equivalence"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/scheduler/util"
	"k8s.io/kubernetes/pkg/scheduler/volumebinder"
	storagelisters "k8s.io/client-go/listers/storage/v1"

	"github.com/golang/glog"
	"fmt"
	"os"
	"io/ioutil"
	kubeschedulerconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/apimachinery/pkg/runtime"
	latestschedulerapi "k8s.io/kubernetes/pkg/scheduler/api/latest"
	"os/signal"
)

// Binder knows how to write a binding.
type Binder interface {
	Bind(binding *v1.Binding) error
}

// PodConditionUpdater updates the condition of a pod based on the passed
// PodCondition
type PodConditionUpdater interface {
	Update(pod *v1.Pod, podCondition *v1.PodCondition) error
}

// PodPreemptor has methods needed to delete a pod and to update
// annotations of the preemptor pod.
type PodPreemptor interface {
	GetUpdatedPod(pod *v1.Pod) (*v1.Pod, error)
	DeletePod(pod *v1.Pod) error
	SetNominatedNodeName(pod *v1.Pod, nominatedNode string) error
	RemoveNominatedNodeName(pod *v1.Pod) error
}

// Scheduler watches for new unscheduled pods. It attempts to find
// nodes that they fit on and writes bindings back to the api server.
type Scheduler struct {
	// It is expected that changes made via SchedulerCache will be observed
	// by NodeLister and Algorithm.
	SchedulerCache schedulercache.Cache
	// Ecache is used for optimistically invalid affected cache items after
	// successfully binding a pod
	Ecache     *equivalence.Cache
	NodeLister algorithm.NodeLister
	Algorithm  algorithm.ScheduleAlgorithm
	GetBinder  func(pod *v1.Pod) Binder
	// PodConditionUpdater is used only in case of scheduling errors. If we succeed
	// with scheduling, PodScheduled condition will be updated in apiserver in /bind
	// handler so that binding and setting PodCondition it is atomic.
	PodConditionUpdater PodConditionUpdater
	// PodPreemptor is used to evict pods and update pod annotations.
	PodPreemptor PodPreemptor

	// NextPod should be a function that blocks until the next pod
	// is available. We don't use a channel for this, because scheduling
	// a pod may take some amount of time and we don't want pods to get
	// stale while they sit in a channel.
	NextPod func() *v1.Pod

	// WaitForCacheSync waits for scheduler cache to populate.
	// It returns true if it was successful, false if the controller should shutdown.
	WaitForCacheSync func() bool

	// Error is called if there is an error. It is passed the pod in
	// question, and the error
	Error func(*v1.Pod, error)

	// Recorder is the EventRecorder to use
	Recorder record.EventRecorder

	// Close this to shut down the scheduler.
	StopEverything chan struct{}

	// VolumeBinder handles PVC/PV binding for the pod.
	VolumeBinder *volumebinder.VolumeBinder

	// Disable pod preemption or not.
	DisablePreemption bool
}

// StopEverything closes the scheduler config's StopEverything channel, to shut
// down the Scheduler.
func (sched *Scheduler) Stop() {
	close(sched.StopEverything)
}

// Cache returns the cache in scheduler for test to check the data in scheduler.
func (sched *Scheduler) Cache() schedulercache.Cache {
	return sched.SchedulerCache
}

// Configurator defines I/O, caching, and other functionality needed to
// construct a new scheduler. An implementation of this can be seen in
// factory.go.
type Configurator interface {
	// Exposed for testing
	GetHardPodAffinitySymmetricWeight() int32
	// Exposed for testing
	MakeDefaultErrorFunc(backoff *util.PodBackoff, podQueue core.SchedulingQueue) func(pod *v1.Pod, err error)

	// Predicate related accessors to be exposed for use by k8s.io/autoscaler/cluster-autoscaler
	GetPredicateMetadataProducer() (algorithm.PredicateMetadataProducer, error)
	GetPredicates(predicateKeys sets.String) (map[string]algorithm.FitPredicate, error)

	// Needs to be exposed for things like integration tests where we want to make fake nodes.
	GetNodeLister() corelisters.NodeLister
	// Exposed for testing
	GetClient() clientset.Interface
	// Exposed for testing
	GetScheduledPodLister() corelisters.PodLister
}

type schedulerOptions struct {
	schedulerName                  string
	hardPodAffinitySymmetricWeight int32
	enableEquivalenceClassCache    bool
	disablePreemption              bool
	percentageOfNodesToScore       int32
	bindTimeoutSeconds             int64
	schedulerAlgorithmSource       kubeschedulerconfig.SchedulerAlgorithmSource
}

// Option is function for schedulerOptions to set value
type Option func(*schedulerOptions)

// WithSchedulerName set schedulerName for Scheduler
func WithSchedulerName(r string) Option {
	return func(o *schedulerOptions) {
		o.schedulerName = r
	}
}

// WithHardPodAffinitySymmetricWeight set hardPodAffinitySymmetricWeight for Scheduler
func WithHardPodAffinitySymmetricWeight(r int32) Option {
	return func(o *schedulerOptions) {
		o.hardPodAffinitySymmetricWeight = r
	}
}

// WithEnableEquivalenceClassCache set enableEquivalenceClassCache for Scheduler
func WithEnableEquivalenceClassCache(r bool) Option {
	return func(o *schedulerOptions) {
		o.enableEquivalenceClassCache = r
	}
}

// WithDisablePreemption set disablePreemption for Scheduler
func WithDisablePreemption(r bool) Option {
	return func(o *schedulerOptions) {
		o.disablePreemption = r
	}
}

// WithPercentageOfNodesToScore set percentageOfNodesToScore for Scheduler
func WithPercentageOfNodesToScore(r int32) Option {
	return func(o *schedulerOptions) {
		o.percentageOfNodesToScore = r
	}
}

// WithBindTimeoutSeconds set bindTimeoutSeconds for Scheduler
func WithBindTimeoutSeconds(r int64) Option {
	return func(o *schedulerOptions) {
		o.bindTimeoutSeconds = r
	}
}

// WithSchedulerAlgorithmSource set schedulerAlgorithmSource for Scheduler
func WithSchedulerAlgorithmSource(r kubeschedulerconfig.SchedulerAlgorithmSource) Option {
	return func(o *schedulerOptions) {
		o.schedulerAlgorithmSource = r
	}
}

var defaultSchedulerOptions = schedulerOptions{
	schedulerName:                  v1.DefaultSchedulerName,
	hardPodAffinitySymmetricWeight: v1.DefaultHardPodAffinitySymmetricWeight,
	enableEquivalenceClassCache:    false,
	disablePreemption:              false,
	percentageOfNodesToScore:       schedulerapi.DefaultPercentageOfNodesToScore,
	bindTimeoutSeconds:             kubeschedulerconfig.BindTimeoutSeconds,
	schedulerAlgorithmSource:       kubeschedulerconfig.SchedulerAlgorithmSource{},
}

// New is constructor for scheduler
// TODO：Once we have the nice constructor，we should modify cmd/kube-scheduler to use it.
func New(client clientset.Interface,
	nodeInformer coreinformers.NodeInformer,
	podInformer coreinformers.PodInformer,
	pvInformer coreinformers.PersistentVolumeInformer,
	pvcInformer coreinformers.PersistentVolumeClaimInformer,
	replicationControllerInformer coreinformers.ReplicationControllerInformer,
	replicaSetInformer appsinformers.ReplicaSetInformer,
	statefulSetInformer appsinformers.StatefulSetInformer,
	serviceInformer coreinformers.ServiceInformer,
	pdbInformer policyinformers.PodDisruptionBudgetInformer,
	storageClassInformer storageinformers.StorageClassInformer,
	recorder record.EventRecorder,
	opts ...func(o *schedulerOptions)) (*Scheduler, error) {

	options := defaultSchedulerOptions
	for _, opt := range opts {
		opt(&options)
	}

	// Set up the configurator which can create schedulers from configs.
	//configurator := factory.NewConfigFactory(&factory.ConfigFactoryArgs{
	//	SchedulerName:                  options.schedulerName,
	//	Client:                         client,
	//	NodeInformer:                   nodeInformer,
	//	PodInformer:                    podInformer,
	//	PvInformer:                     pvInformer,
	//	PvcInformer:                    pvcInformer,
	//	ReplicationControllerInformer:  replicationControllerInformer,
	//	ReplicaSetInformer:             replicaSetInformer,
	//	StatefulSetInformer:            statefulSetInformer,
	//	ServiceInformer:                serviceInformer,
	//	PdbInformer:                    pdbInformer,
	//	StorageClassInformer:           storageClassInformer,
	//	HardPodAffinitySymmetricWeight: options.hardPodAffinitySymmetricWeight,
	//	EnableEquivalenceClassCache:    options.enableEquivalenceClassCache,
	//	DisablePreemption:              options.disablePreemption,
	//	PercentageOfNodesToScore:       options.percentageOfNodesToScore,
	//	BindTimeoutSeconds:             options.bindTimeoutSeconds,
	//})

	// start ----------------------------------------------------------------------------
	// start // move factory NewConfigFactory to here
	stopEverything := make(chan struct{})
	schedulerCache := schedulercache.New(30*time.Second, stopEverything)

	// storageClassInformer is only enabled through VolumeScheduling feature gate
	var storageClassLister storagelisters.StorageClassLister
	if args.StorageClassInformer != nil {
		storageClassLister = args.StorageClassInformer.Lister()
	}
	c := &configFactory{
		client:                         args.Client,
		podLister:                      schedulerCache,
		podQueue:                       core.NewSchedulingQueue(),
		storageClassLister:             storageClassLister,
		schedulerCache:                 schedulerCache,
		StopEverything:                 stopEverything,
		schedulerName:                  args.SchedulerName,
		hardPodAffinitySymmetricWeight: args.HardPodAffinitySymmetricWeight,
		enableEquivalenceClassCache:    args.EnableEquivalenceClassCache,
		disablePreemption:              args.DisablePreemption,
		percentageOfNodesToScore:       args.PercentageOfNodesToScore,
	}

	scheduler := &Scheduler{
		SchedulerCache: schedulerCache,
		Ecache     *equivalence.Cache
		NodeLister algorithm.NodeLister
		Algorithm  algorithm.ScheduleAlgorithm
		GetBinder  func(pod *v1.Pod) Binder
		PodConditionUpdater PodConditionUpdater
		PodPreemptor PodPreemptor
		NextPod func() *v1.Pod
		WaitForCacheSync func() bool
		Error func(*v1.Pod, error)
		Recorder record.EventRecorder
		StopEverything: stopEverything,
		VolumeBinder *volumebinder.VolumeBinder
		DisablePreemption: options.disablePreemption,
	}

	go func() {
		for {
			select {
			case <-s.StopEverything:
				return
			}
		}
	}()
	// end // move factory NewConfigFactory to here
	// end ----------------------------------------------------------------------------

	var config *Config
	source := options.schedulerAlgorithmSource
	switch {
	case source.Provider != nil:
		// Create the config from a named algorithm provider.
		sc, err := configurator.CreateFromProvider(*source.Provider)
		if err != nil {
			return nil, fmt.Errorf("couldn't create scheduler using provider %q: %v", *source.Provider, err)
		}
		config = sc
	case source.Policy != nil:
		// Create the config from a user specified policy source.
		policy := &schedulerapi.Policy{}
		switch {
		case source.Policy.File != nil:
			// Use a policy serialized in a file.
			policyFile := source.Policy.File.Path
			_, err := os.Stat(policyFile)
			if err != nil {
				return nil, fmt.Errorf("missing policy config file %s", policyFile)
			}
			data, err := ioutil.ReadFile(policyFile)
			if err != nil {
				return nil, fmt.Errorf("couldn't read policy config: %v", err)
			}
			err = runtime.DecodeInto(latestschedulerapi.Codec, []byte(data), policy)
			if err != nil {
				return nil, fmt.Errorf("invalid policy: %v", err)
			}
		case source.Policy.ConfigMap != nil:
			// Use a policy serialized in a config map value.
			policyRef := source.Policy.ConfigMap
			policyConfigMap, err := client.CoreV1().ConfigMaps(policyRef.Namespace).Get(policyRef.Name, metav1.GetOptions{})
			if err != nil {
				return nil, fmt.Errorf("couldn't get policy config map %s/%s: %v", policyRef.Namespace, policyRef.Name, err)
			}
			data, found := policyConfigMap.Data[kubeschedulerconfig.SchedulerPolicyConfigMapKey]
			if !found {
				return nil, fmt.Errorf("missing policy config map value at key %q", kubeschedulerconfig.SchedulerPolicyConfigMapKey)
			}
			err = runtime.DecodeInto(latestschedulerapi.Codec, []byte(data), policy)
			if err != nil {
				return nil, fmt.Errorf("invalid policy: %v", err)
			}
		}
		sc, err := configurator.CreateFromConfig(*policy)
		if err != nil {
			return nil, fmt.Errorf("couldn't create scheduler from policy: %v", err)
		}
		config = sc
	default:
		return nil, fmt.Errorf("unsupported algorithm source: %v", source)
	}

	// start ----------------------------------------------------------------------------
	// Create creates a scheduler with the default algorithm provider.
	func (c *configFactory) Create() (*scheduler.Config, error) {
		return c.CreateFromProvider(DefaultProvider)
	}

	// Creates a scheduler from the name of a registered algorithm provider.
	func (c *configFactory) CreateFromProvider(providerName string) (*scheduler.Config, error) {
		glog.V(2).Infof("Creating scheduler from algorithm provider '%v'", providerName)
		provider, err := GetAlgorithmProvider(providerName)
			if err != nil {
			return nil, err
		}
		return c.CreateFromKeys(provider.FitPredicateKeys, provider.PriorityFunctionKeys, []algorithm.SchedulerExtender{})
	}
	// end ----------------------------------------------------------------------------


	// start ----------------------------------------------------------------------------
	// start // move factory CreateFromKeys here
	glog.V(2).Infof("Creating scheduler with fit predicates '%v' and priority functions '%v'", predicateKeys, priorityKeys)

	if c.GetHardPodAffinitySymmetricWeight() < 1 || c.GetHardPodAffinitySymmetricWeight() > 100 {
		return nil, fmt.Errorf("invalid hardPodAffinitySymmetricWeight: %d, must be in the range 1-100", c.GetHardPodAffinitySymmetricWeight())
	}

	predicateFuncs, err := c.GetPredicates(predicateKeys)
	if err != nil {
		return nil, err
	}

	priorityConfigs, err := c.GetPriorityFunctionConfigs(priorityKeys)
	if err != nil {
		return nil, err
	}

	priorityMetaProducer, err := c.GetPriorityMetadataProducer()
	if err != nil {
		return nil, err
	}

	predicateMetaProducer, err := c.GetPredicateMetadataProducer()
	if err != nil {
		return nil, err
	}

	// Init equivalence class cache
	if c.enableEquivalenceClassCache {
		c.equivalencePodCache = equivalence.NewCache()
		glog.Info("Created equivalence class cache")
	}

	algo := core.NewGenericScheduler(
		c.schedulerCache,
		c.equivalencePodCache,
		c.podQueue,
		predicateFuncs,
		predicateMetaProducer,
		priorityConfigs,
		priorityMetaProducer,
		extenders,
		c.volumeBinder,
		c.pVCLister,
		c.alwaysCheckAllPredicates,
		c.disablePreemption,
		c.percentageOfNodesToScore,
	)

	podBackoff := util.CreateDefaultPodBackoff()
	return &scheduler.Config{
		SchedulerCache: c.schedulerCache,
		Ecache:         c.equivalencePodCache,
		// The scheduler only needs to consider schedulable nodes.
		NodeLister:          &nodeLister{c.nodeLister},
		Algorithm:           algo,
		GetBinder:           c.getBinderFunc(extenders),
		PodConditionUpdater: &podConditionUpdater{c.client},
		PodPreemptor:        &podPreemptor{c.client},
		WaitForCacheSync: func() bool {
			return cache.WaitForCacheSync(c.StopEverything, c.scheduledPodsHasSynced)
		},
		NextPod: func() *v1.Pod {
			return c.getNextPod()
		},
		Error:          c.MakeDefaultErrorFunc(podBackoff, c.podQueue),
		StopEverything: c.StopEverything,
		VolumeBinder:   c.volumeBinder,
	}, nil

	// end // move factory CreateFromKeys here
	// start ----------------------------------------------------------------------------

	// Additional tweaks to the config produced by the configurator.
	config.Recorder = recorder
	config.DisablePreemption = options.disablePreemption
	// Create the scheduler.
	sched := NewFromConfig(config)
	return sched, nil
}

// Run begins watching and scheduling. It waits for cache to be synced, then starts a goroutine and returns immediately.
func (sched *Scheduler) Run() {
	if !sched.WaitForCacheSync() {
		return
	}

	go wait.Until(sched.scheduleOne, 0, sched.StopEverything)
}

// schedule implements the scheduling algorithm and returns the suggested host.
func (sched *Scheduler) schedule(pod *v1.Pod) (string, error) {
	host, err := sched.Algorithm.Schedule(pod, sched.NodeLister)
	if err != nil {
		pod = pod.DeepCopy()
		sched.Error(pod, err)
		sched.Recorder.Eventf(pod, v1.EventTypeWarning, "FailedScheduling", "%v", err)
		sched.PodConditionUpdater.Update(pod, &v1.PodCondition{
			Type:    v1.PodScheduled,
			Status:  v1.ConditionFalse,
			Reason:  v1.PodReasonUnschedulable,
			Message: err.Error(),
		})
		return "", err
	}
	return host, err
}

// preempt tries to create room for a pod that has failed to schedule, by preempting lower priority pods if possible.
// If it succeeds, it adds the name of the node where preemption has happened to the pod annotations.
// It returns the node name and an error if any.
func (sched *Scheduler) preempt(preemptor *v1.Pod, scheduleErr error) (string, error) {
	if !util.PodPriorityEnabled() || sched.DisablePreemption {
		glog.V(3).Infof("Pod priority feature is not enabled or preemption is disabled by scheduler configuration." +
			" No preemption is performed.")
		return "", nil
	}
	preemptor, err := sched.PodPreemptor.GetUpdatedPod(preemptor)
	if err != nil {
		glog.Errorf("Error getting the updated preemptor pod object: %v", err)
		return "", err
	}

	node, victims, nominatedPodsToClear, err := sched.Algorithm.Preempt(preemptor, sched.NodeLister, scheduleErr)
	metrics.PreemptionVictims.Set(float64(len(victims)))
	if err != nil {
		glog.Errorf("Error preempting victims to make room for %v/%v.", preemptor.Namespace, preemptor.Name)
		return "", err
	}
	var nodeName = ""
	if node != nil {
		nodeName = node.Name
		err = sched.PodPreemptor.SetNominatedNodeName(preemptor, nodeName)
		if err != nil {
			glog.Errorf("Error in preemption process. Cannot update pod %v/%v annotations: %v", preemptor.Namespace, preemptor.Name, err)
			return "", err
		}
		for _, victim := range victims {
			if err := sched.PodPreemptor.DeletePod(victim); err != nil {
				glog.Errorf("Error preempting pod %v/%v: %v", victim.Namespace, victim.Name, err)
				return "", err
			}
			sched.Recorder.Eventf(victim, v1.EventTypeNormal, "Preempted", "by %v/%v on node %v", preemptor.Namespace, preemptor.Name, nodeName)
		}
	}
	// Clearing nominated pods should happen outside of "if node != nil". Node could
	// be nil when a pod with nominated node name is eligible to preempt again,
	// but preemption logic does not find any node for it. In that case Preempt()
	// function of generic_scheduler.go returns the pod itself for removal of the annotation.
	for _, p := range nominatedPodsToClear {
		rErr := sched.PodPreemptor.RemoveNominatedNodeName(p)
		if rErr != nil {
			glog.Errorf("Cannot remove nominated node annotation of pod: %v", rErr)
			// We do not return as this error is not critical.
		}
	}
	return nodeName, err
}

// assumeVolumes will update the volume cache with the chosen bindings
//
// This function modifies assumed if volume binding is required.
func (sched *Scheduler) assumeVolumes(assumed *v1.Pod, host string) (allBound bool, err error) {
	if utilfeature.DefaultFeatureGate.Enabled(features.VolumeScheduling) {
		allBound, err = sched.VolumeBinder.Binder.AssumePodVolumes(assumed, host)
		if err != nil {
			sched.Error(assumed, err)
			sched.Recorder.Eventf(assumed, v1.EventTypeWarning, "FailedScheduling", "AssumePodVolumes failed: %v", err)
			sched.PodConditionUpdater.Update(assumed, &v1.PodCondition{
				Type:    v1.PodScheduled,
				Status:  v1.ConditionFalse,
				Reason:  "SchedulerError",
				Message: err.Error(),
			})
		}
		// Invalidate ecache because assumed volumes could have affected the cached
		// pvs for other pods
		if sched.Ecache != nil {
			invalidPredicates := sets.NewString(predicates.CheckVolumeBindingPred)
			sched.Ecache.InvalidatePredicates(invalidPredicates)
		}
	}
	return
}

// bindVolumes will make the API update with the assumed bindings and wait until
// the PV controller has completely finished the binding operation.
//
// If binding errors, times out or gets undone, then an error will be returned to
// retry scheduling.
func (sched *Scheduler) bindVolumes(assumed *v1.Pod) error {
	var reason string
	var eventType string

	glog.V(5).Infof("Trying to bind volumes for pod \"%v/%v\"", assumed.Namespace, assumed.Name)
	err := sched.VolumeBinder.Binder.BindPodVolumes(assumed)
	if err != nil {
		glog.V(1).Infof("Failed to bind volumes for pod \"%v/%v\": %v", assumed.Namespace, assumed.Name, err)

		// Unassume the Pod and retry scheduling
		if forgetErr := sched.SchedulerCache.ForgetPod(assumed); forgetErr != nil {
			glog.Errorf("scheduler cache ForgetPod failed: %v", forgetErr)
		}

		reason = "VolumeBindingFailed"
		eventType = v1.EventTypeWarning
		sched.Error(assumed, err)
		sched.Recorder.Eventf(assumed, eventType, "FailedScheduling", "%v", err)
		sched.PodConditionUpdater.Update(assumed, &v1.PodCondition{
			Type:   v1.PodScheduled,
			Status: v1.ConditionFalse,
			Reason: reason,
		})
		return err
	}

	glog.V(5).Infof("Success binding volumes for pod \"%v/%v\"", assumed.Namespace, assumed.Name)
	return nil
}

// assume signals to the cache that a pod is already in the cache, so that binding can be asynchronous.
// assume modifies `assumed`.
func (sched *Scheduler) assume(assumed *v1.Pod, host string) error {
	// Optimistically assume that the binding will succeed and send it to apiserver
	// in the background.
	// If the binding fails, scheduler will release resources allocated to assumed pod
	// immediately.
	assumed.Spec.NodeName = host
	// NOTE: Because the scheduler uses snapshots of SchedulerCache and the live
	// version of Ecache, updates must be written to SchedulerCache before
	// invalidating Ecache.
	if err := sched.SchedulerCache.AssumePod(assumed); err != nil {
		glog.Errorf("scheduler cache AssumePod failed: %v", err)

		// This is most probably result of a BUG in retrying logic.
		// We report an error here so that pod scheduling can be retried.
		// This relies on the fact that Error will check if the pod has been bound
		// to a node and if so will not add it back to the unscheduled pods queue
		// (otherwise this would cause an infinite loop).
		sched.Error(assumed, err)
		sched.Recorder.Eventf(assumed, v1.EventTypeWarning, "FailedScheduling", "AssumePod failed: %v", err)
		sched.PodConditionUpdater.Update(assumed, &v1.PodCondition{
			Type:    v1.PodScheduled,
			Status:  v1.ConditionFalse,
			Reason:  "SchedulerError",
			Message: err.Error(),
		})
		return err
	}

	// Optimistically assume that the binding will succeed, so we need to invalidate affected
	// predicates in equivalence cache.
	// If the binding fails, these invalidated item will not break anything.
	if sched.Ecache != nil {
		sched.Ecache.InvalidateCachedPredicateItemForPodAdd(assumed, host)
	}
	return nil
}

// bind binds a pod to a given node defined in a binding object.  We expect this to run asynchronously, so we
// handle binding metrics internally.
func (sched *Scheduler) bind(assumed *v1.Pod, b *v1.Binding) error {
	bindingStart := time.Now()
	// If binding succeeded then PodScheduled condition will be updated in apiserver so that
	// it's atomic with setting host.
	err := sched.GetBinder(assumed).Bind(b)
	if err := sched.SchedulerCache.FinishBinding(assumed); err != nil {
		glog.Errorf("scheduler cache FinishBinding failed: %v", err)
	}
	if err != nil {
		glog.V(1).Infof("Failed to bind pod: %v/%v", assumed.Namespace, assumed.Name)
		if err := sched.SchedulerCache.ForgetPod(assumed); err != nil {
			glog.Errorf("scheduler cache ForgetPod failed: %v", err)
		}
		sched.Error(assumed, err)
		sched.Recorder.Eventf(assumed, v1.EventTypeWarning, "FailedScheduling", "Binding rejected: %v", err)
		sched.PodConditionUpdater.Update(assumed, &v1.PodCondition{
			Type:   v1.PodScheduled,
			Status: v1.ConditionFalse,
			Reason: "BindingRejected",
		})
		return err
	}

	metrics.BindingLatency.Observe(metrics.SinceInMicroseconds(bindingStart))
	metrics.SchedulingLatency.WithLabelValues(metrics.Binding).Observe(metrics.SinceInSeconds(bindingStart))
	sched.Recorder.Eventf(assumed, v1.EventTypeNormal, "Scheduled", "Successfully assigned %v/%v to %v", assumed.Namespace, assumed.Name, b.Target.Name)
	return nil
}

// scheduleOne does the entire scheduling workflow for a single pod.  It is serialized on the scheduling algorithm's host fitting.
func (sched *Scheduler) scheduleOne() {
	pod := sched.NextPod()
	if pod.DeletionTimestamp != nil {
		sched.Recorder.Eventf(pod, v1.EventTypeWarning, "FailedScheduling", "skip schedule deleting pod: %v/%v", pod.Namespace, pod.Name)
		glog.V(3).Infof("Skip schedule deleting pod: %v/%v", pod.Namespace, pod.Name)
		return
	}

	glog.V(3).Infof("Attempting to schedule pod: %v/%v", pod.Namespace, pod.Name)

	// Synchronously attempt to find a fit for the pod.
	start := time.Now()
	suggestedHost, err := sched.schedule(pod)
	if err != nil {
		// schedule() may have failed because the pod would not fit on any host, so we try to
		// preempt, with the expectation that the next time the pod is tried for scheduling it
		// will fit due to the preemption. It is also possible that a different pod will schedule
		// into the resources that were preempted, but this is harmless.
		if fitError, ok := err.(*core.FitError); ok {
			preemptionStartTime := time.Now()
			sched.preempt(pod, fitError)
			metrics.PreemptionAttempts.Inc()
			metrics.SchedulingAlgorithmPremptionEvaluationDuration.Observe(metrics.SinceInMicroseconds(preemptionStartTime))
			metrics.SchedulingLatency.WithLabelValues(metrics.PreemptionEvaluation).Observe(metrics.SinceInSeconds(preemptionStartTime))
		}
		return
	}
	metrics.SchedulingAlgorithmLatency.Observe(metrics.SinceInMicroseconds(start))
	// Tell the cache to assume that a pod now is running on a given node, even though it hasn't been bound yet.
	// This allows us to keep scheduling without waiting on binding to occur.
	assumedPod := pod.DeepCopy()

	// Assume volumes first before assuming the pod.
	//
	// If all volumes are completely bound, then allBound is true and binding will be skipped.
	//
	// Otherwise, binding of volumes is started after the pod is assumed, but before pod binding.
	//
	// This function modifies 'assumedPod' if volume binding is required.
	allBound, err := sched.assumeVolumes(assumedPod, suggestedHost)
	if err != nil {
		return
	}

	// assume modifies `assumedPod` by setting NodeName=suggestedHost
	err = sched.assume(assumedPod, suggestedHost)
	if err != nil {
		return
	}
	// bind the pod to its host asynchronously (we can do this b/c of the assumption step above).
	go func() {
		// Bind volumes first before Pod
		if !allBound {
			err = sched.bindVolumes(assumedPod)
			if err != nil {
				return
			}
		}

		err := sched.bind(assumedPod, &v1.Binding{
			ObjectMeta: metav1.ObjectMeta{Namespace: assumedPod.Namespace, Name: assumedPod.Name, UID: assumedPod.UID},
			Target: v1.ObjectReference{
				Kind: "Node",
				Name: suggestedHost,
			},
		})
		metrics.E2eSchedulingLatency.Observe(metrics.SinceInMicroseconds(start))
		if err != nil {
			glog.Errorf("Internal error binding pod: (%v)", err)
		}
	}()
}
