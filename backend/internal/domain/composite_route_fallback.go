package domain

// CompositeRouteFallback is one ordered fallback target of a composite route.
// An empty TargetPlatform reuses the route's own target platform.
type CompositeRouteFallback struct {
	TargetPlatform string `json:"target_platform,omitempty"`
	UpstreamModel  string `json:"upstream_model"`
}
