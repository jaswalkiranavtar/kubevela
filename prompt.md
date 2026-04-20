# Requirement
Implement a new KubeVela policy called ocm-topology:
1. The policy should be very similar to toplogy policy with clusters set to local.
2. This policy will be used to deploy KubeVela application to multiple managedclusters joining using ocm open-cluster-management.
3. This policy should wrapp the existing KubeVela application in an OCM MaifestWork resource and drop it in the managedcluster namespaces present in clusters property.
4. When this policy is evaluated, only this and override plicy should be processed in the KubeVela application under processing.
5. All other policies, traits and workflow steps should not be processed and present as it is in the KubeVela application wrapped in ManifestWork.


# Test
Share the instruction on:
1. Building KubeVela after the feature is implemented.
2. So that I can test it in local kind cluster.
