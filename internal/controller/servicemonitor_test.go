package controller_test

import (
	"context"
	api "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	controller "github.com/fjsxy/monitor-in-hand/internal/controller"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ktesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var _ = Describe("可选配套 API 调谐", func() {
	It("UT-SM-GH86-003 Reconcile 创建配套对象并以独立观察结果回报", func() {
		By("以公开 CR 和独立客户端执行真实 controller 入口")
		scheme := runtime.NewScheme()
		Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
		Expect(api.AddToScheme(scheme)).To(Succeed())
		cr := &api.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: "gh86", UID: types.UID("owner86"), Generation: 1}, Spec: api.KubeStateMetricsSpec{TargetNamespace: "monitoring", ServiceMonitor: &api.KubeStateMetricsServiceMonitor{Enabled: true}}}
		c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(cr).WithObjects(cr, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "monitoring"}}).Build()
		r := &controller.KubeStateMetricsReconciler{Client: c, ServerVersion: fixedServerVersion{}, DynamicClient: dynamicfake.NewSimpleDynamicClient(scheme)}
		result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(c.Get(context.Background(), client.ObjectKey{Name: cr.Name}, cr)).To(Succeed())
		Expect(cr.Status.ServiceMonitor).NotTo(BeNil())
		Expect(cr.Status.ServiceMonitor.State).To(Equal("Ready"))
		dc := r.DynamicClient.(*dynamicfake.FakeDynamicClient)
		gvr := schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "servicemonitors"}
		endpoint := dc.Resource(gvr).Namespace("monitoring")
		reconcile := func() {
			_, e := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cr.Name}})
			Expect(e).NotTo(HaveOccurred())
			Expect(c.Get(context.Background(), client.ObjectKey{Name: cr.Name}, cr)).To(Succeed())
		}
		By("修改配套对象后只修复当前 controller owner，权限失败保持 workload")
		obj, e := endpoint.Get(context.Background(), "ksm-gh86", metav1.GetOptions{})
		Expect(e).NotTo(HaveOccurred())
		obj.SetLabels(map[string]string{"changed": "yes"})
		_, e = endpoint.Update(context.Background(), obj, metav1.UpdateOptions{})
		Expect(e).NotTo(HaveOccurred())
		reconcile()
		deny := true
		dc.PrependReactor("get", "servicemonitors", func(action ktesting.Action) (bool, runtime.Object, error) {
			if deny {
				return true, nil, apierrors.NewForbidden(gvr.GroupResource(), "ksm-gh86", nil)
			}
			return false, nil, nil
		})
		reconcile()
		Expect(cr.Status.ServiceMonitor.Reason).To(Equal("PermissionDenied"))
		Expect(c.Get(context.Background(), client.ObjectKey{Namespace: "monitoring", Name: "ksm-gh86"}, &appsv1.Deployment{})).To(Succeed())
		deny = false
		reconcile()
		Expect(cr.Status.ServiceMonitor.State).To(Equal("Ready"))
		By("外部替代身份不能被接管；移除冲突后自动恢复")
		obj, e = endpoint.Get(context.Background(), "ksm-gh86", metav1.GetOptions{})
		Expect(e).NotTo(HaveOccurred())
		obj.SetOwnerReferences(nil)
		_, e = endpoint.Update(context.Background(), obj, metav1.UpdateOptions{})
		Expect(e).NotTo(HaveOccurred())
		reconcile()
		Expect(cr.Status.ServiceMonitor.Reason).To(Equal("ResourceConflict"))
		Expect(endpoint.Delete(context.Background(), "ksm-gh86", metav1.DeleteOptions{})).To(Succeed())
		reconcile()
		Expect(cr.Status.ServiceMonitor.State).To(Equal("Ready"))
		By("挂起保留配套对象，关闭采集只删除配套对象")
		yes := true
		cr.Spec.Suspended = &yes
		Expect(c.Update(context.Background(), cr)).To(Succeed())
		reconcile()
		reconcile()
		Expect(cr.Status.ServiceMonitor.State).To(Equal("Ready"))
		cr.Spec.ServiceMonitor.Enabled = false
		Expect(c.Update(context.Background(), cr)).To(Succeed())
		reconcile()
		reconcile()
		Expect(cr.Status.ServiceMonitor.State).To(Equal("Disabled"))
		By("重新开启后停用清理全部资源")
		cr.Spec.ServiceMonitor.Enabled = true
		Expect(c.Update(context.Background(), cr)).To(Succeed())
		reconcile()
		no := false
		cr.Spec.Enabled = &no
		Expect(c.Update(context.Background(), cr)).To(Succeed())
		reconcile()
		reconcile()
		Expect(cr.Status.ServiceMonitor.State).To(Equal("Disabled"))

	})
})
