"ocm-spoke-application": {
	type: "component"
	annotations: {}
	labels: {}
	description: "Deploys a KubeVela Application to an OCM-managed spoke cluster by wrapping it in a ManifestWork."
	attributes: {
		workload: type: "autodetects.core.oam.dev"
		status: {
			customStatus: #"""
				_feedbacks: *[] | [...{...}]
				if context.output.status != _|_
				if context.output.status.resourceStatus != _|_
				if context.output.status.resourceStatus.manifests != _|_
				if len(context.output.status.resourceStatus.manifests) > 0
				if context.output.status.resourceStatus.manifests[0].statusFeedback != _|_
				if context.output.status.resourceStatus.manifests[0].statusFeedback.values != _|_ {
					_feedbacks: context.output.status.resourceStatus.manifests[0].statusFeedback.values
				}
				_spokePhase: *"unknown" | string
				for v in _feedbacks if v.name == "status" {
					_spokePhase: v.fieldValue.string
				}
				message: "spoke application phase: \(_spokePhase)"
				"""#
			healthPolicy: #"""
				_feedbacks: *[] | [...{...}]
				if context.output.status != _|_
				if context.output.status.resourceStatus != _|_
				if context.output.status.resourceStatus.manifests != _|_
				if len(context.output.status.resourceStatus.manifests) > 0
				if context.output.status.resourceStatus.manifests[0].statusFeedback != _|_
				if context.output.status.resourceStatus.manifests[0].statusFeedback.values != _|_ {
					_feedbacks: context.output.status.resourceStatus.manifests[0].statusFeedback.values
				}
				_spokePhase: *"" | string
				for v in _feedbacks if v.name == "status" {
					_spokePhase: v.fieldValue.string
				}
				isHealth: _spokePhase == "running"
				"""#
		}
	}
}

template: {
	// feedbackRules for the spoke Application resource: surface its phase and conditions.
	_appManifestConfig: {
		resourceIdentifier: {
			group:     "core.oam.dev"
			resource:  "applications"
			namespace: parameter.appSpec.metadata.namespace
			name:      parameter.appSpec.metadata.name
		}
		feedbackRules: [
			{type: "WellKnownStatus"},
			{
				type: "JSONPaths"
				jsonPaths: [
					{name: "status", path: ".status.status"},
					{name: "observedGeneration", path: ".status.observedGeneration"},
					{name: "workflowPhase", path: ".status.workflow.phase"},
					{name: "workflowFinished", path: ".status.workflow.finished"},
				]
			},
		]
	}

	output: {
		apiVersion: "work.open-cluster-management.io/v1"
		kind:       "ManifestWork"
		metadata: {
			name:      context.name
			namespace: parameter.clusterNamespace
		}
		spec: {
			workload: manifests: [parameter.appSpec]
			manifestConfigs: [_appManifestConfig] + parameter.manifestConfigs
		}
	}

	parameter: {
		// +usage=OCM ManagedCluster namespace to target (e.g. "cluster1")
		clusterNamespace: string
		// +usage=The KubeVela Application spec to deploy on the spoke cluster
		appSpec: {
			apiVersion: string
			kind:       string
			metadata: {
				name:      string
				namespace: string
				...
			}
			spec: {}
			...
		}
		// +usage=Additional manifestConfigs for spoke resources (e.g. Deployments). The spoke Application config is always included.
		manifestConfigs: *[] | [...{
			resourceIdentifier: {
				group:     string
				resource:  string
				namespace: string
				name:      string
			}
			feedbackRules: [...{
				type: string
				...
			}]
		}]
	}
}
