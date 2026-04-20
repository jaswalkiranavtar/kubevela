"ocm-topology": {
	annotations: {}
	description: "Deploy application to OCM managed clusters via ManifestWork resources."
	labels: {}
	attributes: {}
	type: "policy"
}

template: {
	parameter: {
		// +usage=Specify the names of the OCM managed clusters to deploy to.
		clusters?: [...string]
		// +usage=Specify the label selector for OCM managed clusters.
		clusterLabelSelector?: [string]: string
		// +usage=Ignore empty cluster error when no cluster returned for label selector.
		allowEmpty?: bool
		// +usage=Specify the target namespace to deploy the wrapped Application in the managed clusters.
		namespace?: string
	}
}
