import (
	"vela/ocm"
	"vela/builtin"
)

"deploy-ocm": {
	type: "workflow-step"
	annotations: {
		"category": "Application Delivery"
	}
	labels: {
		"scope": "Application"
	}
	description: "Deploy application to OCM managed clusters via ManifestWork resources."
}
template: {
	if parameter.auto == false {
		suspend: builtin.#Suspend & {$params: message: "Waiting approval to the deploy-ocm step \"\(context.stepName)\""}
	}
	deploy: ocm.#DeployOCM & {
		$params: {
			policies:    parameter.policies
			parallelism: parameter.parallelism
		}
	}
	parameter: {
		//+usage=If set to false, the workflow will suspend automatically before this step, default to be true.
		auto: *true | bool
		//+usage=Declare the policies that used for this deployment. Should include ocm-topology and optionally override policies.
		policies: *[] | [...string]
		//+usage=Maximum number of concurrent delivered clusters.
		parallelism: *5 | int
	}
}
