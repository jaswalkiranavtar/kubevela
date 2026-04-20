// ocm.cue

#DeployOCM: {
	#provider: "ocm"
	#do:       "deploy-ocm"

	$params: {
		policies: [...string]
		parallelism: int
	}
	$returns?: {...}
}

#ListManagedClusters: {
	#provider: "ocm"
	#do:       "list-managed-clusters"

	$params: {
		policies: [...string]
	}
	$returns?: {
		clusters: [...string]
	}
}
