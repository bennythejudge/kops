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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"k8s.io/kops/cmd/kops/util"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/pkg/commands/commandutils"
	"k8s.io/kops/pkg/dump"
	"k8s.io/kops/pkg/resources"
	resourceops "k8s.io/kops/pkg/resources/ops"
	"k8s.io/kops/upup/pkg/fi/cloudup"
	"k8s.io/kubectl/pkg/util/i18n"
	"k8s.io/kubectl/pkg/util/templates"
)

var (
	toolboxDumpLong = templates.LongDesc(i18n.T(`
	Displays cluster information.  Includes information about cloud and Kubernetes resources.`))

	toolboxDumpExample = templates.Examples(i18n.T(`
	# Dump cluster information
	kops toolbox dump --name k8s-cluster.example.com
	`))

	toolboxDumpShort = i18n.T(`Dump cluster information`)
)

type ToolboxDumpOptions struct {
	Output string

	ClusterName string

	Dir        string
	PrivateKey string
	SSHUser    string
}

func (o *ToolboxDumpOptions) InitDefaults() {
	o.Output = OutputYaml
	o.PrivateKey = "~/.ssh/id_rsa"
	o.SSHUser = "ubuntu"
}

func NewCmdToolboxDump(f *util.Factory, out io.Writer) *cobra.Command {
	options := &ToolboxDumpOptions{}
	options.InitDefaults()

	cmd := &cobra.Command{
		Use:               "dump [CLUSTER]",
		Short:             toolboxDumpShort,
		Long:              toolboxDumpLong,
		Example:           toolboxDumpExample,
		Args:              rootCommand.clusterNameArgs(&options.ClusterName),
		ValidArgsFunction: commandutils.CompleteClusterName(&rootCommand, true, false),
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunToolboxDump(context.TODO(), f, out, options)
		},
	}

	cmd.Flags().StringVarP(&options.Output, "output", "o", options.Output, "Output format.  One of json or yaml")
	cmd.RegisterFlagCompletionFunc("output", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"json", "yaml"}, cobra.ShellCompDirectiveNoFileComp
	})

	cmd.Flags().StringVar(&options.Dir, "dir", options.Dir, "Target directory; if specified will collect logs and other information.")
	cmd.MarkFlagDirname("dir")
	cmd.Flags().StringVar(&options.PrivateKey, "private-key", options.PrivateKey, "File containing private key to use for SSH access to instances")
	cmd.Flags().StringVar(&options.SSHUser, "ssh-user", options.SSHUser, "The remote user for SSH access to instances")
	cmd.RegisterFlagCompletionFunc("ssh-user", cobra.NoFileCompletions)

	return cmd
}

func RunToolboxDump(ctx context.Context, f *util.Factory, out io.Writer, options *ToolboxDumpOptions) error {

	clientset, err := f.Clientset()
	if err != nil {
		return err
	}

	cluster, err := clientset.GetCluster(ctx, options.ClusterName)
	if err != nil {
		return err
	}

	if cluster == nil {
		return fmt.Errorf("cluster not found %q", options.ClusterName)
	}

	cloud, err := cloudup.BuildCloud(cluster)
	if err != nil {
		return err
	}

	region := "" // Use default
	resourceMap, err := resourceops.ListResources(cloud, cluster, region)
	if err != nil {
		return err
	}
	d, err := resources.BuildDump(ctx, cloud, resourceMap)
	if err != nil {
		return err
	}

	if options.Dir != "" {
		privateKeyPath := options.PrivateKey
		if strings.HasPrefix(privateKeyPath, "~/") {
			privateKeyPath = filepath.Join(os.Getenv("HOME"), privateKeyPath[2:])
		}
		key, err := ioutil.ReadFile(privateKeyPath)
		if err != nil {
			return fmt.Errorf("error reading private key %q: %v", privateKeyPath, err)
		}

		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return fmt.Errorf("error parsing private key %q: %v", privateKeyPath, err)
		}

		contextName := cluster.ObjectMeta.Name
		clientGetter := genericclioptions.NewConfigFlags(true)
		clientGetter.Context = &contextName

		var nodes corev1.NodeList

		config, err := clientGetter.ToRESTConfig()
		if err != nil {
			klog.Warningf("cannot load kubeconfig settings for %q: %v", contextName, err)
		} else {
			k8sClient, err := kubernetes.NewForConfig(config)
			if err != nil {
				klog.Warningf("cannot build kube client for %q: %v", contextName, err)
			} else {

				nodeList, err := k8sClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
				if err != nil {
					klog.Warningf("error listing nodes in cluster: %v", err)
				} else {
					nodes = *nodeList
				}
			}
		}

		sshConfig := &ssh.ClientConfig{
			Config: ssh.Config{},
			User:   options.SSHUser,
			Auth: []ssh.AuthMethod{
				ssh.PublicKeys(signer),
			},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		}

		dumper := dump.NewLogDumper(sshConfig, options.Dir)

		var additionalIPs []string
		for _, instance := range d.Instances {
			if len(instance.PublicAddresses) != 0 {
				additionalIPs = append(additionalIPs, instance.PublicAddresses[0])
				continue
			}

			klog.Warningf("no public IP for node %q", instance.Name)
		}

		if err := dumper.DumpAllNodes(ctx, nodes, additionalIPs); err != nil {
			return fmt.Errorf("error dumping nodes: %v", err)
		}
	}

	switch options.Output {
	case OutputYaml:
		b, err := kops.ToRawYaml(d)
		if err != nil {
			return fmt.Errorf("error marshaling yaml: %v", err)
		}
		_, err = out.Write(b)
		if err != nil {
			return fmt.Errorf("error writing to stdout: %v", err)
		}
		return nil

	case OutputJSON:
		b, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			return fmt.Errorf("error marshaling json: %v", err)
		}
		_, err = out.Write(b)
		if err != nil {
			return fmt.Errorf("error writing to stdout: %v", err)
		}
		return nil

	default:
		return fmt.Errorf("unsupported output format: %q", options.Output)
	}
}
