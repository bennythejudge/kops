/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/kops/cmd/kops/util"
	kopsapi "k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/apis/kops/validation"
	"k8s.io/kops/pkg/commands/commandutils"
	"k8s.io/kops/pkg/featureflag"
	"k8s.io/kops/pkg/kopscodecs"
	"k8s.io/kops/pkg/try"
	"k8s.io/kops/upup/pkg/fi/cloudup"
	"k8s.io/kubectl/pkg/cmd/util/editor"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

type CreateInstanceGroupOptions struct {
	ClusterName       string
	InstanceGroupName string
	Role              string
	Subnets           []string
	// DryRun mode output an ig manifest of Output type.
	DryRun bool
	// Output type during a DryRun
	Output string
	// Edit will launch an editor when creating an instance group
	Edit bool
}

var (
	createInstanceGroupLong = templates.LongDesc(i18n.T(`
		Create an InstanceGroup configuration.

	    An InstanceGroup is a group of similar virtual machines.
		On AWS, an InstanceGroup maps to an AutoScalingGroup.

		The Role of an InstanceGroup defines whether machines will act as a Kubernetes master or node.`))

	createInstanceGroupExample = templates.Examples(i18n.T(`

		# Create an instancegroup for the k8s-cluster.example.com cluster.
		kops create instancegroup --name=k8s-cluster.example.com node-example \
		  --role node --subnet my-subnet-name,my-other-subnet-name

		# Create a YAML manifest for an instancegroup for the k8s-cluster.example.com cluster.
		kops create instancegroup --name=k8s-cluster.example.com node-example \
		  --role node --subnet my-subnet-name --dry-run -oyaml
		`))

	createInstanceGroupShort = i18n.T(`Create an instancegroup.`)
)

// NewCmdCreateInstanceGroup create a new cobra command object for creating a instancegroup.
func NewCmdCreateInstanceGroup(f *util.Factory, out io.Writer) *cobra.Command {
	options := &CreateInstanceGroupOptions{
		Role: string(kopsapi.InstanceGroupRoleNode),
		Edit: true,
	}

	cmd := &cobra.Command{
		Use:     "instancegroup INSTANCE_GROUP",
		Aliases: []string{"instancegroups", "ig"},
		Short:   createInstanceGroupShort,
		Long:    createInstanceGroupLong,
		Example: createInstanceGroupExample,
		Args: func(cmd *cobra.Command, args []string) error {
			options.ClusterName = rootCommand.ClusterName(true)

			if options.ClusterName == "" {
				return fmt.Errorf("--name is required")
			}

			if len(args) == 0 {
				return fmt.Errorf("must specify name of instance group to create")
			}

			options.InstanceGroupName = args[0]

			if len(args) != 1 {
				return fmt.Errorf("can only create one instance group at a time")
			}

			return nil
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			commandutils.ConfigureKlogForCompletion()
			if len(args) == 1 && rootCommand.ClusterName(false) == "" {
				return []string{"--name"}, cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunCreateInstanceGroup(context.TODO(), f, out, options)
		},
	}

	allRoles := make([]string, 0, len(kopsapi.AllInstanceGroupRoles))
	for _, r := range kopsapi.AllInstanceGroupRoles {
		if r == kopsapi.InstanceGroupRoleAPIServer && !featureflag.APIServerNodes.Enabled() {
			continue
		}
		allRoles = append(allRoles, strings.ToLower(string(r)))
	}

	cmd.Flags().StringVar(&options.Role, "role", options.Role, "Type of instance group to create ("+strings.Join(allRoles, ",")+")")
	cmd.RegisterFlagCompletionFunc("role", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return allRoles, cobra.ShellCompDirectiveNoFileComp
	})
	cmd.Flags().StringSliceVar(&options.Subnets, "subnet", options.Subnets, "Subnet in which to create instance group. One of Availability Zone like eu-west-1a or a comma-separated list of multiple Availability Zones.")
	cmd.RegisterFlagCompletionFunc("subnet", completeClusterSubnet(&options.Subnets))
	// DryRun mode that will print YAML or JSON
	cmd.Flags().BoolVar(&options.DryRun, "dry-run", options.DryRun, "Only print the object that would be created, without created it. This flag can be used to create an instance group YAML or JSON manifest.")
	cmd.Flags().StringVarP(&options.Output, "output", "o", options.Output, "Output format. One of json or yaml")
	cmd.RegisterFlagCompletionFunc("output", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"json", "yaml"}, cobra.ShellCompDirectiveNoFileComp
	})
	cmd.Flags().BoolVar(&options.Edit, "edit", options.Edit, "Open an editor to edit default values")

	return cmd
}

func RunCreateInstanceGroup(ctx context.Context, f *util.Factory, out io.Writer, options *CreateInstanceGroupOptions) error {
	cluster, err := GetCluster(ctx, f, options.ClusterName)
	if err != nil {
		return fmt.Errorf("error getting cluster: %q: %v", options.ClusterName, err)
	}

	clientset, err := rootCommand.Clientset()
	if err != nil {
		return err
	}

	channel, err := cloudup.ChannelForCluster(cluster)
	if err != nil {
		klog.Warningf("%v", err)
	}

	existing, err := clientset.InstanceGroupsFor(cluster).Get(ctx, options.InstanceGroupName, metav1.GetOptions{})
	if err != nil {
		// We expect a NotFound error when creating the instance group
		if !errors.IsNotFound(err) {
			return err
		}
	}

	if existing != nil {
		return fmt.Errorf("instance group %q already exists", options.InstanceGroupName)
	}

	// Populate some defaults
	ig := &kopsapi.InstanceGroup{}
	ig.ObjectMeta.Name = options.InstanceGroupName

	role, ok := kopsapi.ParseInstanceGroupRole(options.Role, true)
	if !ok {
		return fmt.Errorf("unknown role %q", options.Role)
	}
	ig.Spec.Role = role

	ig.Spec.Subnets = options.Subnets

	cloud, err := cloudup.BuildCloud(cluster)
	if err != nil {
		return err
	}

	ig, err = cloudup.PopulateInstanceGroupSpec(cluster, ig, cloud, channel)
	if err != nil {
		return err
	}

	ig.AddInstanceGroupNodeLabel()
	if kopsapi.CloudProviderID(cluster.Spec.CloudProvider) == kopsapi.CloudProviderGCE {
		fmt.Println("detected a GCE cluster; labeling nodes to receive metadata-proxy.")
		ig.Spec.NodeLabels["cloud.google.com/metadata-proxy-ready"] = "true"
	}

	if options.DryRun {

		if options.Output == "" {
			return fmt.Errorf("must set output flag; yaml or json")
		}

		// Cluster name is not populated, and we need it
		ig.ObjectMeta.Labels = make(map[string]string)
		ig.ObjectMeta.Labels[kopsapi.LabelClusterName] = cluster.ObjectMeta.Name

		switch options.Output {
		case OutputYaml:
			if err := fullOutputYAML(out, ig); err != nil {
				return fmt.Errorf("error writing cluster yaml to stdout: %v", err)
			}
			return nil
		case OutputJSON:
			if err := fullOutputJSON(out, ig); err != nil {
				return fmt.Errorf("error writing cluster json to stdout: %v", err)
			}
			return nil
		default:
			return fmt.Errorf("unsupported output type %q", options.Output)
		}
	}

	if options.Edit {
		var (
			edit = editor.NewDefaultEditor(commandutils.EditorEnvs)
		)

		raw, err := kopscodecs.ToVersionedYaml(ig)
		if err != nil {
			return err
		}
		ext := "yaml"

		// launch the editor
		edited, file, err := edit.LaunchTempFile(fmt.Sprintf("%s-edit-", filepath.Base(os.Args[0])), ext, bytes.NewReader(raw))
		defer func() {
			if file != "" {
				try.RemoveFile(file)
			}
		}()
		if err != nil {
			return fmt.Errorf("error launching editor: %v", err)
		}

		obj, _, err := kopscodecs.Decode(edited, nil)
		if err != nil {
			return fmt.Errorf("error parsing yaml: %v", err)
		}
		group, ok := obj.(*kopsapi.InstanceGroup)
		if !ok {
			return fmt.Errorf("unexpected object type: %T", obj)
		}

		err = validation.CrossValidateInstanceGroup(group, cluster, cloud).ToAggregate()
		if err != nil {
			return err
		}

		ig = group
	}

	_, err = clientset.InstanceGroupsFor(cluster).Create(ctx, ig, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error storing InstanceGroup: %v", err)
	}

	return nil
}

func completeClusterSubnet(excludeSubnets *[]string) func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		commandutils.ConfigureKlogForCompletion()
		ctx := context.TODO()

		cluster, _, completions, directive := GetClusterForCompletion(ctx, &rootCommand, nil)
		if cluster == nil {
			return completions, directive
		}

		if len(args) > 1 {
			return commandutils.CompletionError("too many arguments", nil)
		}

		var requiredType kopsapi.SubnetType
		var subnets []string
		alreadySelected := sets.NewString(*excludeSubnets...)
		for _, subnet := range cluster.Spec.Subnets {
			if alreadySelected.Has(subnet.Name) {
				requiredType = subnet.Type
			}
		}
		for _, subnet := range cluster.Spec.Subnets {
			if !alreadySelected.Has(subnet.Name) && subnet.Type != kopsapi.SubnetTypeUtility &&
				(subnet.Type == requiredType || requiredType == "") {
				subnets = append(subnets, subnet.Name)
			}
		}

		return subnets, cobra.ShellCompDirectiveNoFileComp
	}
}
