"ocm-topology": {
	annotations: {}
	labels: {}
	description: "Fan out a KubeVela Application to N OCM managed clusters, wrapping the Application in a ManifestWork per cluster with optional per-cluster component overrides."
	attributes: scope: "Application"
	type: "policy"
}

template: {
	_spokeAppName: context.appName

	// Strip this policy so the cloned spoke Application does not recurse through the same transform.
	_spokePolicies: [for p in context.appPolicies if p.name != context.policyName {p}] & [...{}]

	// Per-cluster effective component list: for each base component, apply the matching override (if any).
	// Override semantics (v1): properties and traits are each fully replaced when provided; otherwise the
	// base component's value is kept. Other fields (dependsOn, inputs, outputs, externalRevision) flow through.
	_componentsFor: {
		for c in parameter.clusters {
			"\(c.name)": [
				for comp in context.appComponents {
					let _matches = [for o in c.overrides if o.component == comp.name {o}]
					if len(_matches) == 0 {comp}
					if len(_matches) > 0 {
						{
							name: comp.name
							type: comp.type
							if _matches[0].properties != _|_ {
								properties: _matches[0].properties
							}
							if _matches[0].properties == _|_ {
								if comp.properties != _|_ {
									properties: comp.properties
								}
							}
							if _matches[0].traits != _|_ {
								traits: _matches[0].traits
							}
							if _matches[0].traits == _|_ {
								if comp.traits != _|_ {
									traits: comp.traits
								}
							}
							if comp.dependsOn != _|_ {
								dependsOn: comp.dependsOn
							}
							if comp.inputs != _|_ {
								inputs: comp.inputs
							}
							if comp.outputs != _|_ {
								outputs: comp.outputs
							}
							if comp.externalRevision != _|_ {
								externalRevision: comp.externalRevision
							}
						}
					}
				},
			]
		}
	}

	output: {
		labels: "app.oam.dev/managed-by": "ocm"

		// One ocm-spoke-application component per target cluster, each wrapping
		// that cluster's effective spoke Application spec in a ManifestWork
		// placed in the cluster's hub-side namespace.
		components: [
			for c in parameter.clusters {
				name: "ocm-\(_spokeAppName)-\(c.name)"
				type: "ocm-spoke-application"
				properties: {
					clusterNamespace: c.name
					appSpec: {
						apiVersion: "core.oam.dev/v1beta1"
						kind:       "Application"
						metadata: {
							name:      _spokeAppName
							namespace: context.namespace
						}
						spec: {
							components: _componentsFor[c.name]
							if len(_spokePolicies) > 0 {
								policies: _spokePolicies
							}
							if context.appWorkflow != null {
								workflow: context.appWorkflow
							}
						}
					}
				}
			},
		]

		// Hub Application carries no business policies; they all live on the spoke.
		policies: []
	}

	parameter: {
		// +usage=Target OCM managed clusters with optional per-cluster overrides.
		clusters: [...{
			// +usage=OCM ManagedCluster name (also its hub-side namespace).
			name: string
			// +usage=Optional per-cluster component overrides. Each entry matches a
			// base component by name and fully replaces its properties and/or traits.
			// Components without a matching override fall through to the base spec.
			overrides: *[] | [...{
				// +usage=Base component name to override.
				component: string
				// +usage=Full replacement for the component's properties. Omit to keep base properties.
				properties?: {...}
				// +usage=Full replacement for the component's traits list. Omit to keep base traits.
				traits?: [...{
					type: string
					properties?: {...}
				}]
			}]
		}]
	}
}
