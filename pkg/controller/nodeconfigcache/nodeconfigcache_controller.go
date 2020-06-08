package nodeconfigcache

import (
	"context"
	"fmt"

	cachesv1alpha1 "github.com/3scale/marin3r/pkg/apis/caches/v1alpha1"
	xds_cache "github.com/envoyproxy/go-control-plane/pkg/cache/v2"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_nodeconfigcache")

// Add creates a new NodeConfigCache Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, cache *xds_cache.SnapshotCache) error {
	return add(mgr, newReconciler(mgr, cache))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, c *xds_cache.SnapshotCache) reconcile.Reconciler {
	return &ReconcileNodeConfigCache{
		client:   mgr.GetClient(),
		scheme:   mgr.GetScheme(),
		adsCache: c,
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("nodeconfigcache-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	filter := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			// Ignore updates to CR status in which case metadata.Generation does not change
			if e.MetaOld.GetGeneration() == e.MetaNew.GetGeneration() {
				// But trigger reconciles on condition updates as this is the way
				// other controllers communicate with this one
				if !apiequality.Semantic.DeepEqual(
					e.ObjectOld.(*cachesv1alpha1.NodeConfigCache).Status.Conditions,
					e.ObjectNew.(*cachesv1alpha1.NodeConfigCache).Status.Conditions,
				) {
					return true
				}
				return false
			}
			return true
		},
	}
	// Watch for changes to primary resource NodeConfigCache
	err = c.Watch(&source.Kind{Type: &cachesv1alpha1.NodeConfigCache{}}, &handler.EnqueueRequestForObject{}, filter)
	if err != nil {
		return err
	}

	// Watch for owned resources NodeConfigRevision
	err = c.Watch(&source.Kind{Type: &cachesv1alpha1.NodeConfigRevision{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &cachesv1alpha1.NodeConfigCache{},
	})

	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileNodeConfigCache implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileNodeConfigCache{}

// ReconcileNodeConfigCache reconciles a NodeConfigCache object
type ReconcileNodeConfigCache struct {
	client   client.Client
	scheme   *runtime.Scheme
	adsCache *xds_cache.SnapshotCache
}

// Reconcile reads that state of the cluster for a NodeConfigCache object and makes changes based on the state read
// and what is in the NodeConfigCache.Spec
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileNodeConfigCache) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling NodeConfigCache")

	ctx := context.TODO()

	// Fetch the NodeConfigCache instance
	ncc := &cachesv1alpha1.NodeConfigCache{}
	err := r.client.Get(ctx, request.NamespacedName, ncc)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// Check if the NodeConfigCache instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	if ncc.GetDeletionTimestamp() != nil {
		if contains(ncc.GetFinalizers(), cachesv1alpha1.NodeConfigCacheFinalizer) {
			r.finalizeNodeConfigCache(ncc.Spec.NodeID)
			reqLogger.V(1).Info("Successfully cleared ads server cache")
			// Remove memcachedFinalizer. Once all finalizers have been
			// removed, the object will be deleted.
			controllerutil.RemoveFinalizer(ncc, cachesv1alpha1.NodeConfigCacheFinalizer)
			err := r.client.Update(ctx, ncc)
			if err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	// TODO: add the label with the nodeID if it is missing

	// Add finalizer for this CR
	if !contains(ncc.GetFinalizers(), cachesv1alpha1.NodeConfigCacheFinalizer) {
		reqLogger.Info("Adding Finalizer for the NodeConfigCache")
		if err := r.addFinalizer(ctx, ncc); err != nil {
			reqLogger.Error(err, "Failed adding finalizer for nodecacheconfig")
			return reconcile.Result{}, err
		}
	}

	// desiredVersion is the version that matches the resources described in the spec
	desiredVersion := calculateRevisionHash(ncc.Spec.Resources)

	// ensure that the desiredVersion has a matching revision object
	if err := r.ensureNodeConfigRevision(ctx, ncc, desiredVersion); err != nil {
		return reconcile.Result{}, err
	}

	// Update the ConfigRevisions list in the status
	if err := r.consolidateRevisionList(ctx, ncc, desiredVersion); err != nil {
		return reconcile.Result{}, err
	}

	// determine the version that should be published
	version, err := r.getVersionToPublish(ctx, ncc)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Mark the revision as published
	if err := r.markRevisionPublished(ctx, ncc.Spec.NodeID, version, "VersionPublished", fmt.Sprintf("Version '%s' has been published", version)); err != nil {
		if err.(cacheError).ErrorType == RevisionTaintedError {
			// The revision that matches the current spec is tainted and
			// cannot be published. Set the "CacheOutOfSync" condition
			if err := r.setCacheOutOfSyncCondition(ctx, ncc, "DesiredRevisionTainted",
				"The revision thas describes the current spec is tainted due to detected failures"); err != nil {
				return reconcile.Result{}, err
			}
			reqLogger.Info("The revision described by the spec is tainted, cannot reconcile", "NodeID", ncc.Spec.NodeID, "Version", version)
			// Do no requeue, the "DesiredRevisionTainted" error needs that the
			// user fixes the resources in the spec
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// Update the status
	if err := r.updateStatus(ctx, ncc, desiredVersion, version); err != nil {
		return reconcile.Result{}, err
	}

	// Cleanup unreferenced NodeConfigRevision objects
	if err := r.deleteUnreferencedRevisions(ctx, ncc); err != nil {
		return reconcile.Result{}, err
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileNodeConfigCache) getVersionToPublish(ctx context.Context, ncc *cachesv1alpha1.NodeConfigCache) (string, error) {
	// Get the list of revisions for this nodeID
	ncrList := &cachesv1alpha1.NodeConfigRevisionList{}
	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{nodeIDTag: ncc.Spec.NodeID},
	})
	if err != nil {
		return "", newCacheError(UnknownError, "deleteUnreferencedRevisions", err.Error())
	}
	err = r.client.List(ctx, ncrList, &client.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", newCacheError(UnknownError, "deleteUnreferencedRevisions", err.Error())
	}

	// Starting from the highest index in the ConfigRevision list and going
	// down, return the first version found that is not tainted
	for i := len(ncc.Status.ConfigRevisions) - 1; i >= 0; i-- {
		for _, ncr := range ncrList.Items {
			if ncc.Status.ConfigRevisions[i].Version == ncr.Spec.Version && !ncr.Status.Conditions.IsTrueFor(cachesv1alpha1.RevisionTaintedCondition) {
				return ncc.Status.ConfigRevisions[i].Version, nil
			}
		}
	}

	// If we get here it means that ther is not untainted revision
	// TODO: set a condition

	return "", nil
}

func (r *ReconcileNodeConfigCache) updateStatus(ctx context.Context, ncc *cachesv1alpha1.NodeConfigCache, desired, published string) error {

	changed := false
	patch := client.MergeFrom(ncc.DeepCopy())

	if ncc.Status.PublishedVersion != published {
		ncc.Status.PublishedVersion = published
		changed = true
	}

	if ncc.Status.DesiredVersion != desired {
		ncc.Status.DesiredVersion = desired
		changed = true
	}

	if desired == published {
		ncc.Status.CacheState = cachesv1alpha1.InSyncState
	} else {
		ncc.Status.CacheState = cachesv1alpha1.RollbackState
	}

	if changed {
		if err := r.client.Status().Patch(ctx, ncc, patch); err != nil {
			return err
		}
	}

	return nil
}

func (r *ReconcileNodeConfigCache) finalizeNodeConfigCache(nodeID string) {
	(*r.adsCache).ClearSnapshot(nodeID)
}

func (r *ReconcileNodeConfigCache) addFinalizer(ctx context.Context, ncc *cachesv1alpha1.NodeConfigCache) error {
	controllerutil.AddFinalizer(ncc, cachesv1alpha1.NodeConfigCacheFinalizer)

	// Update CR
	err := r.client.Update(ctx, ncc)
	if err != nil {
		return err
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
