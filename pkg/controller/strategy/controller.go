package strategy

import (
	"fmt"
	"github.com/golang/glog"
	"github.com/bookingcom/shipper/pkg/apis/shipper/v1"
	clientset "github.com/bookingcom/shipper/pkg/client/clientset/versioned"
	informers "github.com/bookingcom/shipper/pkg/client/informers/externalversions"
	listers "github.com/bookingcom/shipper/pkg/client/listers/shipper/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"os"
	"time"
)

type Controller struct {
	clientset                 *clientset.Clientset
	capacityTargetsLister     listers.CapacityTargetLister
	installationTargetsLister listers.InstallationTargetLister
	trafficTargetsLister      listers.TrafficTargetLister
	releasesLister            listers.ReleaseLister
	releasesSynced            cache.InformerSynced
	capacityTargetsSynced     cache.InformerSynced
	trafficTargetsSynced      cache.InformerSynced
	installationTargetsSynced cache.InformerSynced
	dynamicClientPool         dynamic.ClientPool
	workqueue                 workqueue.RateLimitingInterface
}

func NewController(
	clientset *clientset.Clientset,
	informerFactory informers.SharedInformerFactory,
	restConfig *rest.Config,
) *Controller {

	dynamicClientPool := dynamic.NewDynamicClientPool(restConfig)
	releaseInformer := informerFactory.Shipper().V1().Releases()
	capacityTargetInformer := informerFactory.Shipper().V1().CapacityTargets()
	trafficTargetInformer := informerFactory.Shipper().V1().TrafficTargets()
	installationTargetInformer := informerFactory.Shipper().V1().InstallationTargets()

	controller := &Controller{
		clientset:                 clientset,
		capacityTargetsLister:     informerFactory.Shipper().V1().CapacityTargets().Lister(),
		installationTargetsLister: informerFactory.Shipper().V1().InstallationTargets().Lister(),
		trafficTargetsLister:      informerFactory.Shipper().V1().TrafficTargets().Lister(),
		releasesLister:            releaseInformer.Lister(),
		releasesSynced:            releaseInformer.Informer().HasSynced,
		capacityTargetsSynced:     capacityTargetInformer.Informer().HasSynced,
		trafficTargetsSynced:      trafficTargetInformer.Informer().HasSynced,
		installationTargetsSynced: installationTargetInformer.Informer().HasSynced,
		workqueue:                 workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Releases"),
		dynamicClientPool:         dynamicClientPool,
	}

	releaseInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueRelease,
		UpdateFunc: func(oldObj, newObj interface{}) {
			rel := newObj.(*v1.Release)
			if isWorkingOnStrategy(rel) {
				// We should enqueue only releases that have been modified AND
				// are in the middle of a strategy execution.
				controller.enqueueRelease(rel)
			}
		},
	})

	// The InstallationTarget object should have the same name as the Release
	// object it is associated with.
	installationTargetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueInstallationTarget,
		UpdateFunc: func(oldObj, newObj interface{}) {
			controller.enqueueInstallationTarget(newObj)
		},
	})

	// The CapacityTarget object should have the same name as the Release
	// object it is associated with.
	capacityTargetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueCapacityTarget,
		UpdateFunc: func(oldObj, newObj interface{}) {
			controller.enqueueCapacityTarget(newObj)
		},
	})

	// The TrafficTarget object should have the same name as the Release
	// object it is associate with.
	trafficTargetInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueTrafficTarget,
		UpdateFunc: func(oldObj, newObj interface{}) {
			controller.enqueueTrafficTarget(newObj)
		},
	})

	return controller
}

func isWorkingOnStrategy(r *v1.Release) (workingOnStrategy bool) {
	switch r.Status.Phase {
	case
		v1.ReleasePhaseWaitingForCommand,
		v1.ReleasePhaseWaitingForStrategy:
		workingOnStrategy = true
	default:
		workingOnStrategy = false
	}

	return workingOnStrategy
}

func (c *Controller) contenderForRelease(r *v1.Release) (*v1.Release, error) {
	if contenderName, ok := r.GetAnnotations()[v1.ReleaseContenderAnn]; ok {
		if contender, err := c.releasesLister.Releases(r.Namespace).Get(contenderName); err != nil {
			return nil, err
		} else {
			return contender, nil
		}
	}
	return nil, nil
}

func isInstalled(r *v1.Release) bool {
	return r.Status.Phase == v1.ReleasePhaseInstalled
}

func (c *Controller) getAssociatedRelease(obj *metav1.ObjectMeta) *v1.Release {
	if rel, err := c.releasesLister.Releases(obj.Namespace).Get(obj.Name); err != nil {
		glog.V(4).Infof("error fetching release %s: %s", obj.Name, err)
		return nil
	} else {
		return rel
	}
}

func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	if ok := cache.WaitForCacheSync(stopCh, c.releasesSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	if ok := cache.WaitForCacheSync(stopCh, c.installationTargetsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	if ok := cache.WaitForCacheSync(stopCh, c.trafficTargetsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	if ok := cache.WaitForCacheSync(stopCh, c.capacityTargetsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	glog.Info("Started workers")
	<-stopCh
	glog.Info("Shutting down workers")

	return nil
}

func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	defer c.workqueue.Done(obj)

	if key, ok := obj.(string); !ok {
		c.workqueue.Forget(obj)
		runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
		return false
	} else {
		if err := c.syncOne(key); err != nil {
			runtime.HandleError(fmt.Errorf("error syncing: %q: %s", key, err.Error()))
			return false
		} else {
			c.workqueue.Forget(obj)
			return true
		}
	}
}

func (c *Controller) syncOne(key string) error {
	ns, name, err := cache.SplitMetaNamespaceKey(key)

	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	strategy, err := c.buildStrategy(ns, name)
	if err != nil {
		return err
	}

	strategy.info("will start processing release")
	if result, err := strategy.execute(); err != nil {
		return err
	} else if len(result) > 0 {
		strategy.info("strategy executed, patches to apply")
		for _, e := range result {

			r := e.(ExecutorResult)

			// XXX: This is work in progress. result implements the ExecutorResult
			// interface, and if it is not nil then a patch is required, using the
			// information from the returned gvk, together with the []byte that
			// represents the patch encoded in JSON.
			name, gvk, b := r.Patch()

			if client, err := c.clientForGroupVersionKind(gvk, ns); err != nil {
				return err
			} else if _, err = client.Patch(name, types.MergePatchType, b); err != nil {
				return err
			}
		}
	} else {
		strategy.info("strategy executed, nothing to patch")
	}

	return nil
}

func (c *Controller) clientForGroupVersionKind(
	gvk schema.GroupVersionKind,
	ns string,
) (dynamic.ResourceInterface, error) {
	client, err := c.dynamicClientPool.ClientForGroupVersionKind(gvk)
	if err != nil {
		return nil, err
	}

	// This is sort of stupid, it might exist some better way to get the APIResource here...
	var resource *metav1.APIResource
	gv := gvk.GroupVersion().String()
	if resources, err := c.clientset.Discovery().ServerResourcesForGroupVersion(gv); err != nil {
		return nil, err
	} else {
		for _, r := range resources.APIResources {
			if r.Kind == gvk.Kind {
				resource = &r
				break
			}
		}
	}

	if resource == nil {
		return nil, fmt.Errorf("could not find the specified resource %q", gvk)
	}

	return client.Resource(resource, ns), nil
}

func (c *Controller) buildReleaseInfo(ns string, name string) (*releaseInfo, error) {
	release, err := c.releasesLister.Releases(ns).Get(name)
	if err != nil {
		return nil, err
	}

	installationTarget, err := c.installationTargetsLister.InstallationTargets(ns).Get(name)
	if err != nil {
		return nil, err
	}

	capacityTarget, err := c.capacityTargetsLister.CapacityTargets(ns).Get(name)
	if err != nil {
		return nil, err
	}

	trafficTarget, err := c.trafficTargetsLister.TrafficTargets(ns).Get(name)
	if err != nil {
		return nil, err
	}

	return &releaseInfo{
		release:            release,
		installationTarget: installationTarget,
		trafficTarget:      trafficTarget,
		capacityTarget:     capacityTarget,
	}, nil
}

func (c *Controller) incumbentReleaseNameForRelease(ns string, name string) (string, error) {
	if rel, err := c.releasesLister.Releases(ns).Get(name); err != nil {
		return "", err
	} else if incumbentReleaseName, ok := rel.GetAnnotations()[v1.ReleaseIncumbentAnn]; ok {
		return incumbentReleaseName, nil
	}
	return "", os.ErrNotExist
}

func (c *Controller) buildStrategy(ns string, name string) (*Executor, error) {

	contenderReleaseInfo, err := c.buildReleaseInfo(ns, name)
	if err != nil {
		return nil, err
	}

	var incumbentReleaseInfo *releaseInfo
	if incumbentReleaseName, err := c.incumbentReleaseNameForRelease(ns, name); !os.IsNotExist(err) {
		incumbentReleaseInfo, err = c.buildReleaseInfo(ns, incumbentReleaseName)
		if err != nil {
			return nil, err
		}
	}

	return &Executor{
		contender: contenderReleaseInfo,
		incumbent: incumbentReleaseInfo,
	}, nil
}

func (c *Controller) enqueueInstallationTarget(obj interface{}) {
	it := obj.(*v1.InstallationTarget)
	if rel := c.getAssociatedRelease(&it.ObjectMeta); rel != nil {
		c.enqueueRelease(rel)
	}
}

func (c *Controller) enqueueTrafficTarget(obj interface{}) {
	tt := obj.(*v1.TrafficTarget)
	if rel := c.getAssociatedRelease(&tt.ObjectMeta); rel != nil {
		c.enqueueRelease(rel)
	}
}

func (c *Controller) enqueueCapacityTarget(obj interface{}) {
	ct := obj.(*v1.CapacityTarget)
	if rel := c.getAssociatedRelease(&ct.ObjectMeta); rel != nil {
		c.enqueueRelease(rel)
	}
}

func (c *Controller) enqueueRelease(obj interface{}) {
	rel := obj.(*v1.Release)
	glog.V(5).Infof("inspecting release %s/%s", rel.Namespace, rel.Name)

	if isInstalled(rel) {
		// isInstalled returns true if Release.Status.Phase is Installed. If this
		// is true, it is really likely that a modification was made in an installed
		// release, so we check if there's a contender for this release and enqueue
		// it instead. Now that I think more about it, I'm questioning how often this
		// code path would be executed... Ah, this code path *will* be executed since
		// capacity and traffic target objects will be modified when transitioning from
		// one release to the other. So, this code path will be executed when
		// CapacityTarget, TrafficTarget objects, for both contender and incumbent
		// releases, and all those should enqueue only the contender release in the work
		// queue.

		// Check if there is a contender release for given release.
		if contenderRel, err := c.contenderForRelease(rel); err != nil {
			runtime.HandleError(err)
		} else if contenderRel != nil {

			if isWorkingOnStrategy(contenderRel) {
				if key, err := cache.MetaNamespaceKeyFunc(contenderRel); err != nil {
					runtime.HandleError(err)
				} else {
					glog.V(5).Infof("enqueued item %q", key)
					c.workqueue.AddRateLimited(key)
				}
			}
		} else {
			glog.V(5).Infof("couldn't find a release to enqueue based on %s/%s", rel.Namespace, rel.Name)
		}
	} else if isWorkingOnStrategy(rel) {
		// This release is in the middle of its strategy, so we just enqueue it.
		if key, err := cache.MetaNamespaceKeyFunc(rel); err != nil {
			runtime.HandleError(err)
		} else {
			glog.V(5).Infof("enqueued item %q", key)
			c.workqueue.AddRateLimited(key)
		}
	} else {
		glog.V(5).Infof("couldn't find a release to enqueue based on %s/%s", rel.Namespace, rel.Name)
	}
}
