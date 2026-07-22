// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"net/http/httptest"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestTokenReviewEdgeAuthenticatorRequiresAudienceAndServiceAccount(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset()
	clientset.PrependReactor("create", "tokenreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		create := action.(clienttesting.CreateAction)

		review := create.GetObject().(*authenticationv1.TokenReview)
		if len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != EdgeTokenAudience {
			t.Errorf("audiences = %v", review.Spec.Audiences)
		}

		return true, &authenticationv1.TokenReview{
			ObjectMeta: metav1.ObjectMeta{},
			Status: authenticationv1.TokenReviewStatus{
				Authenticated: true,
				Audiences:     []string{EdgeTokenAudience},
				User:          authenticationv1.UserInfo{Username: "system:serviceaccount:unbounded-system:metalman-edge"},
			},
		}, nil
	})
	authenticator := &TokenReviewEdgeAuthenticator{Client: clientset.AuthenticationV1(), ServiceAccountName: "metalman-edge"}
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set("Authorization", "Bearer edge-token")

	if !authenticator.Authenticate(t.Context(), request) {
		t.Fatal("expected edge token to authenticate")
	}
}

func TestTokenReviewEdgeAuthenticatorRejectsWrongAudience(t *testing.T) {
	t.Parallel()

	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
			Authenticated: true,
			Audiences:     []string{"other"},
			User:          authenticationv1.UserInfo{Username: "system:serviceaccount:unbounded-system:metalman-edge"},
		}}, nil
	})
	authenticator := &TokenReviewEdgeAuthenticator{Client: clientset.AuthenticationV1(), ServiceAccountName: "metalman-edge"}
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set("Authorization", "Bearer edge-token")

	if authenticator.Authenticate(t.Context(), request) {
		t.Fatal("expected wrong audience to be rejected")
	}
}
