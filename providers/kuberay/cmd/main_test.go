package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestInitRegistersSchemes(t *testing.T) {
	if !scheme.Recognizes(schema.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"}) {
		t.Fatal("expected core v1 Pod to be registered in scheme")
	}

	if !scheme.Recognizes(schema.GroupVersionKind{Group: "airunway.ai", Version: "v1alpha1", Kind: "ModelDeployment"}) {
		t.Fatal("expected airunway v1alpha1 ModelDeployment to be registered in scheme")
	}
}
