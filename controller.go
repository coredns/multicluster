package multicluster

import (
	"context"
	"errors"
	"fmt"
	"github.com/coredns/coredns/plugin/kubernetes/object"
	model "github.com/vanekjar/coredns-multicluster/object"
	api "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
	mcs "sigs.k8s.io/mcs-api/pkg/client/clientset/versioned/typed/apis/v1alpha1"
	"sync"
	"sync/atomic"
	"time"
)

const (
	svcNameNamespaceIndex = "ServiceNameNamespace"
)

type controller interface {
	ServiceList() []*model.ServiceImport
	SvcIndex(string) []*model.ServiceImport

	GetNamespaceByName(string) (*object.Namespace, error)

	Run()
	HasSynced() bool
	Stop() error

	// Modified returns the timestamp of the most recent changes
	Modified() int64
}

type control struct {
	// Modified tracks timestamp of the most recent changes
	// It needs to be first because it is guaranteed to be 8-byte
	// aligned ( we use sync.LoadAtomic with this )
	modified int64

	k8sClient kubernetes.Interface
	mcsClient mcs.MulticlusterV1alpha1Interface

	svcImportController cache.Controller
	svcImportLister     cache.Indexer

	nsController cache.Controller
	nsLister     cache.Store

	// stopLock is used to enforce only a single call to Stop is active.
	// Needed because we allow stopping through an http endpoint and
	// allowing concurrent stoppers leads to stack traces.
	stopLock sync.Mutex
	shutdown bool
	stopCh   chan struct{}
}

func newController(ctx context.Context, k8sClient kubernetes.Interface, mcsClient mcs.MulticlusterV1alpha1Interface) *control {
	ctl := control{
		k8sClient: k8sClient,
		mcsClient: mcsClient,
		stopCh:    make(chan struct{}),
	}

	ctl.svcImportLister, ctl.svcImportController = object.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc:  serviceImportListFunc(ctx, ctl.mcsClient, api.NamespaceAll),
			WatchFunc: serviceImportWatchFunc(ctx, ctl.mcsClient, api.NamespaceAll),
		},
		&v1alpha1.ServiceImport{},
		cache.ResourceEventHandlerFuncs{AddFunc: ctl.Add, UpdateFunc: ctl.Update, DeleteFunc: ctl.Delete},
		cache.Indexers{svcNameNamespaceIndex: svcNameNamespaceIndexFunc},
		object.DefaultProcessor(model.ToServiceImport, nil),
	)

	ctl.nsLister, ctl.nsController = object.NewIndexerInformer(
		&cache.ListWatch{
			ListFunc:  namespaceListFunc(ctx, ctl.k8sClient),
			WatchFunc: namespaceWatchFunc(ctx, ctl.k8sClient),
		},
		&api.Namespace{},
		cache.ResourceEventHandlerFuncs{},
		cache.Indexers{},
		object.DefaultProcessor(object.ToNamespace, nil),
	)

	return &ctl
}

// Stop stops the  controller.
func (c *control) Stop() error {
	c.stopLock.Lock()
	defer c.stopLock.Unlock()

	// Only try draining the workqueue if we haven't already.
	if !c.shutdown {
		close(c.stopCh)
		c.shutdown = true

		return nil
	}

	return fmt.Errorf("shutdown already in progress")
}

// Run starts the controller.
func (c *control) Run() {
	go c.svcImportController.Run(c.stopCh)
	go c.nsController.Run(c.stopCh)
	<-c.stopCh
}

// HasSynced calls on all controllers.
func (c *control) HasSynced() bool {
	return c.svcImportController.HasSynced() && c.nsController.HasSynced()
}

func (c *control) SvcIndex(idx string) (svcs []*model.ServiceImport) {
	os, err := c.svcImportLister.ByIndex(svcNameNamespaceIndex, idx)
	if err != nil {
		return nil
	}
	for _, o := range os {
		s, ok := o.(*model.ServiceImport)
		if !ok {
			continue
		}
		svcs = append(svcs, s)
	}
	return svcs
}

func (c *control) ServiceList() (svcs []*model.ServiceImport) {
	os := c.svcImportLister.List()
	for _, o := range os {
		s, ok := o.(*model.ServiceImport)
		if !ok {
			continue
		}
		svcs = append(svcs, s)
	}
	return svcs
}

func serviceImportListFunc(ctx context.Context, c mcs.MulticlusterV1alpha1Interface, ns string) func(meta.ListOptions) (runtime.Object, error) {
	return func(opts meta.ListOptions) (runtime.Object, error) {
		return c.ServiceImports(ns).List(ctx, opts)
	}
}

func serviceImportWatchFunc(ctx context.Context, c mcs.MulticlusterV1alpha1Interface, ns string) func(options meta.ListOptions) (watch.Interface, error) {
	return func(options meta.ListOptions) (watch.Interface, error) {
		return c.ServiceImports(ns).Watch(ctx, options)
	}
}

func namespaceListFunc(ctx context.Context, c kubernetes.Interface) func(meta.ListOptions) (runtime.Object, error) {
	return func(opts meta.ListOptions) (runtime.Object, error) {
		return c.CoreV1().Namespaces().List(ctx, opts)
	}
}

func namespaceWatchFunc(ctx context.Context, c kubernetes.Interface) func(options meta.ListOptions) (watch.Interface, error) {
	return func(options meta.ListOptions) (watch.Interface, error) {
		return c.CoreV1().Namespaces().Watch(ctx, options)
	}
}

// GetNamespaceByName returns the namespace by name. If nothing is found an error is returned.
func (c *control) GetNamespaceByName(name string) (*object.Namespace, error) {
	o, exists, err := c.nsLister.GetByKey(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("namespace not found")
	}
	ns, ok := o.(*object.Namespace)
	if !ok {
		return nil, fmt.Errorf("found key but not namespace")
	}
	return ns, nil
}

func (c *control) Add(obj interface{})               { c.updateModified() }
func (c *control) Delete(obj interface{})            { c.updateModified() }
func (c *control) Update(oldObj, newObj interface{}) { c.detectChanges(oldObj, newObj) }

// detectChanges detects changes in objects, and updates the modified timestamp
func (c *control) detectChanges(oldObj, newObj interface{}) {
	// If both objects have the same resource version, they are identical.
	if newObj != nil && oldObj != nil && (oldObj.(meta.Object).GetResourceVersion() == newObj.(meta.Object).GetResourceVersion()) {
		return
	}
	obj := newObj
	if obj == nil {
		obj = oldObj
	}
	switch ob := obj.(type) {
	case *model.ServiceImport:
		c.updateModified()
	default:
		log.Warningf("Updates for %T not supported.", ob)
	}
}

func (c *control) Modified() int64 {
	unix := atomic.LoadInt64(&c.modified)
	return unix
}

func (c *control) updateModified() {
	unix := time.Now().Unix()
	atomic.StoreInt64(&c.modified, unix)
}

func svcNameNamespaceIndexFunc(obj interface{}) ([]string, error) {
	s, ok := obj.(*model.ServiceImport)
	if !ok {
		return nil, errors.New("obj was not of the correct type")
	}
	return []string{s.Index}, nil
}
