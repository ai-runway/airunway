package v1alpha1

import (
	"fmt"
	"strings"
)

// ValidateImageFields verifies the legacy image override and the engine image
// override do not request different container images.
//
// spec.engine.image is the preferred field for new configurations, while
// spec.image remains supported as a legacy fallback. Setting both fields to the
// same value is allowed for backward compatibility during migration.
func (spec *ModelDeploymentSpec) ValidateImageFields() error {
	if spec.Image != "" && spec.Engine.Image != "" && spec.Image != spec.Engine.Image {
		return fmt.Errorf(
			"spec.image %q conflicts with spec.engine.image %q; remove spec.image or set both fields to the same value (spec.engine.image is preferred)",
			spec.Image,
			spec.Engine.Image,
		)
	}
	return nil
}

// ImageOverride returns the configured image override, preferring the engine
// image field over the legacy top-level image field.
func (spec *ModelDeploymentSpec) ImageOverride() string {
	if spec.Engine.Image != "" {
		return spec.Engine.Image
	}
	return spec.Image
}

// ValidateEngineArgs verifies that a launch flag is not set in BOTH
// spec.engine.args (the structured map) and spec.engine.extraArgs (raw tokens).
//
// engine.args is a map, so a key can appear there at most once; finding the same
// key again in extraArgs is an unambiguous contradiction. Providers that consume
// both fields (Direct vLLM, llm-d) render engine.args first and then append
// extraArgs verbatim, so such a collision would emit two conflicting copies of
// the flag (e.g. "--tensor-parallel-size 4 … --tensor-parallel-size=2"). Engines
// like vLLM parse last-wins, so the extraArgs value would silently defeat the
// engine.args one. Reject the contradiction instead of guessing a winner; the
// user sets the flag in exactly one place.
//
// This is provider-agnostic on purpose: it runs at admission for every
// ModelDeployment (the provider is frequently auto-selected and unknown at
// admission time) and is re-checked by the relevant provider transforms as a
// reconcile-time backstop. Flags that legitimately repeat live only in extraArgs
// and are untouched by this check.
func (spec *ModelDeploymentSpec) ValidateEngineArgs() error {
	if len(spec.Engine.Args) == 0 || len(spec.Engine.ExtraArgs) == 0 {
		return nil
	}
	for _, arg := range spec.Engine.ExtraArgs {
		key, ok := extraArgFlagKey(arg)
		if !ok {
			continue
		}
		if _, dup := spec.Engine.Args[key]; dup {
			return fmt.Errorf(
				"launch flag %q is set in both spec.engine.args and spec.engine.extraArgs (%q); set it in exactly one place so the engine does not receive conflicting values",
				key, arg,
			)
		}
	}
	return nil
}

// extraArgFlagKey extracts the bare flag name from a raw extraArgs token,
// stripping the leading "--" and any "=value" suffix. It returns ok=false for
// tokens that are not "--flag" style (bare values, single-dash tokens, "--"),
// which therefore cannot collide with a structured engine.args key.
func extraArgFlagKey(arg string) (string, bool) {
	if !strings.HasPrefix(arg, "--") || len(arg) <= 2 {
		return "", false
	}
	body := strings.TrimPrefix(arg, "--")
	if strings.HasPrefix(body, "-") {
		return "", false
	}
	if equalIndex := strings.Index(body, "="); equalIndex >= 0 {
		body = body[:equalIndex]
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "", false
	}
	return body, true
}
