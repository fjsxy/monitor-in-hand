package controller_test

import (
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	monitoringv1alpha1 "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
)

var (
	testEnvironment    *envtest.Environment
	environmentStarted bool
	testClient         client.Client
	testScheme         *runtime.Scheme
)

func TestController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "KubeStateMetrics Controller envtest 测试套件")
}

var _ = BeforeSuite(func() {
	testScheme = runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(testScheme)).To(Succeed())
	Expect(appsv1.AddToScheme(testScheme)).To(Succeed())
	Expect(corev1.AddToScheme(testScheme)).To(Succeed())
	Expect(rbacv1.AddToScheme(testScheme)).To(Succeed())
	Expect(monitoringv1alpha1.AddToScheme(testScheme)).To(Succeed())

	crdPath, err := filepath.Abs(filepath.Join("..", "..", "..", "config", "crd", "bases"))
	Expect(err).NotTo(HaveOccurred())
	testEnvironment = &envtest.Environment{CRDDirectoryPaths: []string{crdPath}}
	config, err := testEnvironment.Start()
	Expect(err).NotTo(HaveOccurred())
	environmentStarted = true
	testClient, err = client.New(config, client.Options{Scheme: testScheme})
	Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
	if environmentStarted {
		Expect(testEnvironment.Stop()).To(Succeed())
	}
})
