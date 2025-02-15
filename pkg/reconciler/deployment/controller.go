package deployment

import (
	"context"
	"log"
	"time"

	clusterclient "github.com/kcp-dev/kcp/pkg/client/clientset/versioned"
	"github.com/kcp-dev/kcp/pkg/client/informers/externalversions"
	clusterlisters "github.com/kcp-dev/kcp/pkg/client/listers/cluster/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appsv1client "k8s.io/client-go/kubernetes/typed/apps/v1"
	appsv1lister "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

const resyncPeriod = 10 * time.Hour

// NewController returns a new Controller which splits new Deployment objects
// into N virtual Deployments labeled for each Cluster that exists at the time
// the Deployment is created.
func NewController(cfg *rest.Config) *Controller {
	client := appsv1client.NewForConfigOrDie(cfg)
	kubeClient := kubernetes.NewForConfigOrDie(cfg)
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	sif := informers.NewSharedInformerFactoryWithOptions(kubeClient, resyncPeriod)
	sif.Apps().V1().Deployments().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { queue.AddRateLimited(obj) },
		UpdateFunc: func(_, obj interface{}) { queue.AddRateLimited(obj) },
	})
	stopCh := make(chan struct{}) // TODO: hook this up to SIGTERM/SIGINT
	sif.WaitForCacheSync(stopCh)
	sif.Start(stopCh)

	csif := externalversions.NewSharedInformerFactoryWithOptions(clusterclient.NewForConfigOrDie(cfg), resyncPeriod)
	csif.WaitForCacheSync(stopCh)
	csif.Start(stopCh)

	return &Controller{
		queue:         queue,
		client:        client,
		indexer:       sif.Apps().V1().Deployments().Informer().GetIndexer(),
		lister:        sif.Apps().V1().Deployments().Lister(),
		clusterLister: csif.Cluster().V1alpha1().Clusters().Lister(),
		stopCh:        stopCh,
	}
}

type Controller struct {
	queue         workqueue.RateLimitingInterface
	client        *appsv1client.AppsV1Client
	indexer       cache.Indexer
	lister        appsv1lister.DeploymentLister
	clusterLister clusterlisters.ClusterLister
	kubeClient    kubernetes.Interface
	stopCh        chan struct{}
}

func (c *Controller) Start(numThreads int) {
	defer c.queue.ShutDown()
	for i := 0; i < numThreads; i++ {
		go wait.Until(c.startWorker, time.Second, c.stopCh)
	}
	log.Println("Starting workers")
	<-c.stopCh
	log.Println("Stopping workers")
}

func (c *Controller) startWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	// Wait until there is a new item in the working queue
	k, quit := c.queue.Get()
	if quit {
		return false
	}
	key := k.(string)

	// No matter what, tell the queue we're done with this key, to unblock
	// other workers.
	defer c.queue.Done(key)

	err := c.process(key)
	c.handleErr(err, key)
	return true
}

func (c *Controller) handleErr(err error, key string) {
	// Reconcile worked, nothing else to do for this workqueue item.
	if err == nil {
		c.queue.Forget(key)
		return
	}

	// Re-enqueue up to 5 times.
	num := c.queue.NumRequeues(key)
	if num < 5 {
		log.Printf("Error reconciling key %q, retrying... (#%d): %v", key, num, err)
		c.queue.AddRateLimited(key)
		return
	}

	// Give up and report error elsewhere.
	c.queue.Forget(key)
	runtime.HandleError(err)
	log.Printf("Dropping key %q after failed retries: %v", key, err)
}

func (c *Controller) process(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		return err
	}

	if !exists {
		log.Printf("Object with key %q was deleted", key)
		return nil
	}
	current := obj.(*appsv1.Deployment)
	previous := current.DeepCopy()

	ctx := context.TODO()
	if err := c.reconcile(ctx, current); err != nil {
		return err
	}

	// If the object being reconciled changed as a result, update it.
	if !equality.Semantic.DeepEqual(previous.Status, current.Status) {
		_, uerr := c.client.Deployments(current.Namespace).UpdateStatus(ctx, current, metav1.UpdateOptions{})
		return uerr
	}

	return err
}
