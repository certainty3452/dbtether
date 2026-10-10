package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

func TestCheckReferenceConsent(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		from    string
		wantErr bool
	}{
		{name: "own namespace without list", from: "prod"},
		{name: "empty list denies", from: "dev", wantErr: true},
		{name: "listed", allowed: []string{"dev"}, from: "dev"},
		{name: "unlisted", allowed: []string{"qa"}, from: "dev", wantErr: true},
		{name: "wildcard allows another namespace", allowed: []string{"*"}, from: "dev"},
		{name: "wildcard mixed with names allows any", allowed: []string{"*", "ns-a"}, from: "dev"},
		{name: "prefix pattern allows", allowed: []string{"box-*"}, from: "box-1"},
		{name: "prefix pattern denies missing dash", allowed: []string{"box-*"}, from: "boxx", wantErr: true},
		{name: "prefix pattern denies infix", allowed: []string{"box-*"}, from: "dev-box-1", wantErr: true},
		{name: "suffix pattern allows", allowed: []string{"*-dev"}, from: "customer-api-dev"},
		{name: "suffix pattern denies", allowed: []string{"*-dev"}, from: "customer-api-prod", wantErr: true},
		{name: "infix star matches empty", allowed: []string{"a*b"}, from: "ab"},
		{name: "infix star matches sequence", allowed: []string{"a*b"}, from: "axxb"},
		{name: "infix pattern denies trailing text", allowed: []string{"a*b"}, from: "abx", wantErr: true},
		{name: "two stars", allowed: []string{"team-*-api"}, from: "team-x-api"},
		{name: "two stars deny", allowed: []string{"team-*-api"}, from: "team-x-apix", wantErr: true},
		{name: "mixed list pattern", allowed: []string{"box-*", "customer-api-dev"}, from: "box-7"},
		{name: "mixed list exact", allowed: []string{"box-*", "customer-api-dev"}, from: "customer-api-dev"},
		{name: "mixed list denies", allowed: []string{"box-*", "customer-api-dev"}, from: "customer-api-prod", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "prod"},
				Spec:       databasesv1alpha1.DatabaseSpec{AllowedNamespaces: tt.allowed},
			}
			err := CheckReferenceConsent(db, tt.from)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
