package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openshift-online/maestro/pkg/api/openapi"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("Resources", Ordered, Label("e2e-tests-resources"), func() {

	It("is CRUD tests", func() {
	})

	var resource *openapi.Resource

	Context("Create Resource", func() {

		It("post the configmap resource to the maestro api", func() {
			res := helper.NewAPIResource(consumer_id, "test_value")
			var resp *http.Response
			var err error
			resource, resp, err = apiClient.DefaultApi.ApiMaestroV1ResourcesPost(context.Background()).Resource(res).Execute()
			Expect(err).ShouldNot(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusCreated))
			Expect(*resource.Id).ShouldNot(BeEmpty())

			Eventually(func() error {
				_, err := kubeClient.CoreV1().ConfigMaps("test").Get(context.Background(), "test", metav1.GetOptions{})
				if err != nil {
					return err
				}
				return nil
			}, 1*time.Minute, 1*time.Second).ShouldNot(HaveOccurred())
		})
	})

	Context("Patch Resource", func() {

		It("patch the configmap resource", func() {

			newRes := helper.NewAPIResource(consumer_id, "test_new_value")
			patchedResource, resp, err := apiClient.DefaultApi.ApiMaestroV1ResourcesIdPatch(context.Background(), *resource.Id).
				ResourcePatchRequest(openapi.ResourcePatchRequest{Version: resource.Version, Manifest: newRes.Manifest}).Execute()
			Expect(err).ShouldNot(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusOK))
			Expect(*patchedResource.Version).To(Equal(*resource.Version + 1))

			Eventually(func() error {
				cm, err := kubeClient.CoreV1().ConfigMaps("test").Get(context.Background(), "test", metav1.GetOptions{})
				if err != nil {
					return err
				}
				if cm.Data["test_key"] == "test_new_value" {
					return nil
				}
				return fmt.Errorf("configmap not patched correctly")
			}, 1*time.Minute, 1*time.Second).ShouldNot(HaveOccurred())
		})
	})

	Context("Delete Resource", func() {

		It("delete the nginx resource", func() {

			resp, err := apiClient.DefaultApi.ApiMaestroV1ResourcesIdDelete(context.Background(), *resource.Id).Execute()
			Expect(err).ShouldNot(HaveOccurred())
			Expect(resp.StatusCode).To(Equal(http.StatusNoContent))

			Eventually(func() error {
				_, err := kubeClient.CoreV1().ConfigMaps("test").Get(context.Background(), "test", metav1.GetOptions{})
				if err != nil {
					if errors.IsNotFound(err) {
						return nil
					}
					return err
				}
				return fmt.Errorf("the configmap still exists")
			}, 1*time.Minute, 1*time.Second).ShouldNot(HaveOccurred())
		})
	})

})
