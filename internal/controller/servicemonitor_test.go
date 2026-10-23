package controller_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"net/http"
	"net/http/httptest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sync/atomic"
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
		By("create 已写入但响应丢失时按 owner 核验，不重复创建")
		Expect(endpoint.Delete(context.Background(), "ksm-gh86", metav1.DeleteOptions{})).To(Succeed())
		ambiguous := true
		dc.PrependReactor("create", "servicemonitors", func(action ktesting.Action) (bool, runtime.Object, error) {
			if !ambiguous {
				return false, nil, nil
			}
			ambiguous = false
			obj := action.(ktesting.CreateAction).GetObject()
			Expect(dc.Tracker().Create(gvr, obj, "monitoring")).To(Succeed())
			return true, nil, errors.New("injected response loss after create")
		})
		reconcile()
		Expect(cr.Status.ServiceMonitor.State).To(Equal("Degraded"))
		count := len(dc.Actions())
		reconcile()
		Expect(cr.Status.ServiceMonitor.State).To(Equal("Ready"))
		for _, action := range dc.Actions()[count:] {
			Expect(action.GetVerb()).NotTo(Equal("create"))
		}
		By("并发更新 409 保留身份并在下一轮重试")
		conflictOnce := true
		dc.PrependReactor("update", "servicemonitors", func(action ktesting.Action) (bool, runtime.Object, error) {
			if conflictOnce {
				conflictOnce = false
				return true, nil, apierrors.NewConflict(gvr.GroupResource(), "ksm-gh86", errors.New("replaced RV"))
			}
			return false, nil, nil
		})
		cr.Spec.ServiceMonitor.Interval = "20s"
		Expect(c.Update(context.Background(), cr)).To(Succeed())
		reconcile()
		Expect(cr.Status.ServiceMonitor.State).To(Equal("Degraded"))
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
		deny = true
		reconcile()
		Expect(cr.Status.ServiceMonitor.State).To(Equal("Degraded"))
		Expect(cr.Status.ServiceMonitor.Resource).NotTo(BeNil(), "关闭后的读取失败不能丢失待清理身份")
		deny = false
		obj, e = endpoint.Get(context.Background(), "ksm-gh86", metav1.GetOptions{})
		Expect(e).NotTo(HaveOccurred())
		obj.SetUID("observed-child-uid")
		obj.SetResourceVersion("42")
		_, e = endpoint.Update(context.Background(), obj, metav1.UpdateOptions{})
		Expect(e).NotTo(HaveOccurred())
		By("真实 dynamic HTTP 请求携带 UID/RV 条件；旧 client-go fake 不保留 DeleteOptions")
		deleted := atomic.Bool{}
		deletes := make(chan metav1.DeleteOptions, 1)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if request.Method == http.MethodDelete {
				options := metav1.DeleteOptions{}
				if err := json.NewDecoder(request.Body).Decode(&options); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				deletes <- options
				deleted.Store(true)
				_ = json.NewEncoder(w).Encode(&metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success"})
				return
			}
			if deleted.Load() {
				w.WriteHeader(404)
				_ = json.NewEncoder(w).Encode(&metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404})
				return
			}
			_ = json.NewEncoder(w).Encode(obj)
		}))
		defer server.Close()
		r.DynamicClient, e = dynamic.NewForConfig(&rest.Config{Host: server.URL})
		Expect(e).NotTo(HaveOccurred())
		reconcile()
		options := <-deletes
		Expect(options.Preconditions).NotTo(BeNil())
		Expect(*options.Preconditions.UID).To(Equal(types.UID("observed-child-uid")))
		Expect(*options.Preconditions.ResourceVersion).To(Equal("42"))
		reconcile()
		Expect(cr.Status.ServiceMonitor.State).To(Equal("Disabled"))
		Expect(endpoint.Delete(context.Background(), "ksm-gh86", metav1.DeleteOptions{})).To(Succeed())
		r.DynamicClient = dc
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
