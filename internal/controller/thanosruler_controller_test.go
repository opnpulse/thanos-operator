/*
Copyright 2024.

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

package controller

import (
	"context"
	"fmt"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	monitoringthanosiov1alpha1 "github.com/thanos-community/thanos-operator/api/v1alpha1"
	"github.com/thanos-community/thanos-operator/internal/pkg/manifests"
	"github.com/thanos-community/thanos-operator/test/utils"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
)

var _ = Describe("ThanosRuler Controller", Ordered, func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName = "test-resource"
			ns           = "test-ruler"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: ns,
		}

		BeforeAll(func() {
			By("creating the namespace and objstore secret")
			Expect(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: ns,
				},
			})).Should(Succeed())

			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "thanos-objstore",
					Namespace: ns,
				},
				StringData: map[string]string{
					"thanos.yaml": `type: S3
config:
  bucket: test
  endpoint: http://localhost:9000
  access_key: Cheesecake
  secret_key: supersecret
  http_config:
    insecure_skip_verify: false
`,
				},
			})).Should(Succeed())
		})

		AfterEach(func() {
			resource := &monitoringthanosiov1alpha1.ThanosRuler{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance ThanosRuler")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		It("should reconcile correctly", func() {
			if os.Getenv("EXCLUDE_RULER") == skipValue {
				Skip("Skipping ThanosRuler controller tests")
			}
			resource := &monitoringthanosiov1alpha1.ThanosRuler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: ns,
				},
				Spec: monitoringthanosiov1alpha1.ThanosRulerSpec{
					Replicas:     2,
					CommonFields: monitoringthanosiov1alpha1.CommonFields{},
					StorageSize:  "1Gi",
					ObjectStorageConfig: monitoringthanosiov1alpha1.ObjectStorageConfig{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "thanos-objstore",
						},
						Key: "thanos.yaml",
					},
					PrometheusRuleSelector: metav1.LabelSelector{
						MatchLabels: map[string]string{
							manifests.DefaultPrometheusRuleLabel: manifests.DefaultPrometheusRuleValue,
						},
					},
					AlertmanagerURL: "http://alertmanager.com:9093",
					RuleTenancyConfig: &monitoringthanosiov1alpha1.RuleTenancyConfig{
						TenantLabel:      "tenant",
						TenantValueLabel: "operator.thanos.io/tenant",
					},
					Additional: monitoringthanosiov1alpha1.Additional{
						Containers: []corev1.Container{
							{
								Name:  "jaeger-agent",
								Image: "jaegertracing/jaeger-agent:1.22",
								Args:  []string{"--reporter.grpc.host-port=jaeger-collector:14250"},
							},
						},
					},
				},
			}

			By("setting up the thanos ruler resources", func() {
				svc := &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-query",
						Namespace: ns,
						Labels:    requiredQueryServiceLabels,
					},
					Spec: corev1.ServiceSpec{
						Ports: []corev1.ServicePort{
							{
								Name:       "grpc",
								Port:       10901,
								TargetPort: intstr.FromInt32(10901),
							},
						},
					},
				}
				Expect(k8sClient.Create(context.Background(), svc)).Should(Succeed())

				Expect(k8sClient.Create(context.Background(), resource)).Should(Succeed())
				verifier := utils.Verifier{}.WithServiceAccount().WithService().WithStatefulSet()
				EventuallyWithOffset(1, func() bool {
					return verifier.Verify(k8sClient, RulerNameFromParent(resourceName), ns)
				}, time.Minute, time.Second*2).Should(BeTrue())

				EventuallyWithOffset(1, func() bool {
					return utils.VerifyStatefulSetArgs(k8sClient, RulerNameFromParent(resourceName), ns, 0, "--label=rule_replica=\"$(NAME)\"")
				}, time.Second*30, time.Second*2).Should(BeTrue())

				EventuallyWithOffset(1, func() bool {
					return utils.VerifyStatefulSetReplicas(
						k8sClient, 2, RulerNameFromParent(resourceName), ns)
				}, time.Second*30, time.Second*2).Should(BeTrue())

				EventuallyWithOffset(1, func() bool {
					arg := fmt.Sprintf("--query=dnssrv+_http._tcp.%s.%s.svc.cluster.local", "my-query", ns)
					return utils.VerifyStatefulSetArgs(k8sClient, RulerNameFromParent(resourceName), ns, 0, arg)
				}, time.Minute, time.Second*2).Should(BeTrue())
			})

			By("updating with new rule file", func() {
				cfgmap := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "my-rules",
						Namespace: ns,
						Labels:    requiredRuleConfigMapLabels,
					},
					Data: map[string]string{
						"my-rules.yaml": `groups:
- name: example
  rules:
  - alert: HighRequestLatency
    expr: job:request_latency_seconds:mean5m{job="myjob"} > 0.5
	for: 10m
	labels:
	  severity: page
`,
					},
				}
				Expect(k8sClient.Create(context.Background(), cfgmap)).Should(Succeed())

				promRule := &monitoringv1.PrometheusRule{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-promrule",
						Namespace: ns,
						Labels: map[string]string{
							manifests.DefaultPrometheusRuleLabel: manifests.DefaultPrometheusRuleValue,
							"operator.thanos.io/tenant":          "test",
						},
					},
					Spec: monitoringv1.PrometheusRuleSpec{
						Groups: []monitoringv1.RuleGroup{
							{
								Name: "example",
								Rules: []monitoringv1.Rule{
									{
										Alert: "HighRequestLatency",
										Expr:  intstr.FromString(`job:request_latency_seconds:mean5m{job="myjob"} > 0.5`),
										Labels: map[string]string{
											"severity": "page",
										},
									},
								},
							},
						},
					},
				}
				Expect(k8sClient.Create(context.Background(), promRule)).Should(Succeed())

				EventuallyWithOffset(1, func() bool {
					arg := "--rule-file=/etc/thanos/rules/" + promRule.Name + ".yaml"
					return utils.VerifyStatefulSetArgs(k8sClient, RulerNameFromParent(resourceName), ns, 0, arg)
				}, time.Minute, time.Second*2).Should(BeTrue())

				EventuallyWithOffset(1, func() bool {
					arg := "--rule-file=/etc/thanos/rules/my-rules.yaml"
					return utils.VerifyStatefulSetArgs(k8sClient, RulerNameFromParent(resourceName), ns, 0, arg)
				}, time.Minute, time.Second*2).Should(BeTrue())

				EventuallyWithOffset(1, func() bool {
					cfgmapName := fmt.Sprintf("%s-promrule-0", resourceName)
					return utils.VerifyConfigMapContents(k8sClient, cfgmapName, ns, "test-promrule.yaml",
						`groups:
- labels:
    tenant: test
  name: example
  rules:
  - alert: HighRequestLatency
    expr: job:request_latency_seconds:mean5m{job="myjob",tenant="test"} > 0.5
    labels:
      severity: page
`)
				}, time.Second*10, time.Second*2).Should(BeTrue())

			})

		})
	})
})
