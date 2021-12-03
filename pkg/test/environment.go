package test

import (
	"context"
	"fmt"
	"sync"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	"github.com/phayes/freeport"
	"github.com/rancher/opni-opensearch-operator/api"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

type Reconciler interface {
	SetupWithManager(ctrl.Manager) error
}

var ExternalResources sync.WaitGroup

func RunTestEnvironment(
	testEnv *envtest.Environment,
	runControllerManager bool,
	externalEnv bool,
	reconcilers ...Reconciler,
) (stop context.CancelFunc, k8sManager ctrl.Manager, k8sClient client.Client) {
	if !externalEnv && len(reconcilers) == 0 {
		panic("no reconcilers")
	}
	var ctx context.Context
	ctx, stop = context.WithCancel(ctrl.SetupSignalHandler())

	cfg, err := testEnv.Start()
	gomega.Expect(err).NotTo(gomega.HaveOccurred())
	gomega.Expect(cfg).NotTo(gomega.BeNil())
	ExternalResources.Add(1)

	go func() {
		defer ginkgo.GinkgoRecover()
		defer ExternalResources.Done()
		<-ctx.Done()
		err := testEnv.Stop()
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}()

	if runControllerManager {
		StartControllerManager(ctx, testEnv)
	}

	api.InitScheme(scheme.Scheme)

	ports, err := freeport.GetFreePorts(2)
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	// add the opnicluster manager
	k8sManager, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme.Scheme,
		MetricsBindAddress:     fmt.Sprintf(":%d", ports[0]),
		HealthProbeBindAddress: fmt.Sprintf(":%d", ports[1]),
	})
	gomega.Expect(err).NotTo(gomega.HaveOccurred())

	k8sClient = k8sManager.GetClient()
	gomega.Expect(k8sClient).NotTo(gomega.BeNil())

	for _, rec := range reconcilers {
		gomega.Expect(rec.SetupWithManager(k8sManager)).NotTo(gomega.HaveOccurred())
	}

	go func() {
		defer ginkgo.GinkgoRecover()
		err = k8sManager.Start(ctx)
		gomega.Expect(err).NotTo(gomega.HaveOccurred())
	}()
	return
}
