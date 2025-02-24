// Copyright 2023 The Okteto Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package destroy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	contextCMD "github.com/okteto/okteto/cmd/context"
	"github.com/okteto/okteto/cmd/utils"
	"github.com/okteto/okteto/cmd/utils/executor"
	"github.com/okteto/okteto/pkg/config"
	"github.com/okteto/okteto/pkg/constants"
	"github.com/okteto/okteto/pkg/devenvironment"
	oktetoErrors "github.com/okteto/okteto/pkg/errors"

	"github.com/okteto/okteto/pkg/k8s/kubeconfig"
	"github.com/okteto/okteto/pkg/k8s/namespaces"
	"github.com/okteto/okteto/pkg/k8s/secrets"
	oktetoLog "github.com/okteto/okteto/pkg/log"
	"github.com/okteto/okteto/pkg/model"
	"github.com/okteto/okteto/pkg/okteto"
	oktetoPath "github.com/okteto/okteto/pkg/path"
	"github.com/spf13/cobra"
	v1 "k8s.io/api/core/v1"
)

const (
	ownerLabel           = "owner"
	nameLabel            = "name"
	helmOwner            = "helm"
	helmUninstallCommand = "helm uninstall %s"
)

type destroyer interface {
	DestroyWithLabel(ctx context.Context, ns string, opts namespaces.DeleteAllOptions) error
	DestroySFSVolumes(ctx context.Context, ns string, opts namespaces.DeleteAllOptions) error
}

type secretHandler interface {
	List(ctx context.Context, ns, labelSelector string) ([]v1.Secret, error)
}

// Options destroy commands options
type Options struct {
	// ManifestPathFlag is the option -f as introduced by the user when executing this command.
	// This is stored at the configmap as filename to redeploy from the ui.
	ManifestPathFlag string
	// ManifestPath is the path to the manifest used though the command execution.
	// This might change its value during execution
	ManifestPath        string
	Name                string
	Variables           []string
	Namespace           string
	DestroyVolumes      bool
	DestroyDependencies bool
	ForceDestroy        bool
	K8sContext          string
	RunWithoutBash      bool
	DestroyAll          bool
	RunInRemote         bool
}

type destroyInterface interface {
	destroy(context.Context, *Options) error
}
type destroyCommand struct {
	getManifest func(path string) (*model.Manifest, error)

	executor          executor.ManifestExecutor
	nsDestroyer       destroyer
	secrets           secretHandler
	k8sClientProvider okteto.K8sClientProvider
	ConfigMapHandler  configMapHandler
	oktetoClient      *okteto.OktetoClient
}

// Destroy destroys the dev application defined by the manifest
func Destroy(ctx context.Context) *cobra.Command {
	options := &Options{
		Variables: []string{},
	}

	cmd := &cobra.Command{
		Use:   "destroy",
		Short: `Destroy everything created by the 'okteto deploy' command`,
		Long:  `Destroy everything created by the 'okteto deploy' command. You can also include a 'destroy' section in your okteto manifest with a list of custom commands to be executed on destroy`,
		Args:  utils.NoArgsAccepted("https://okteto.com/docs/reference/cli/#destroy"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if options.ManifestPath != "" {
				// if path is absolute, its transformed to rel from root
				initialCWD, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("failed to get the current working directory: %w", err)
				}
				manifestPathFlag, err := oktetoPath.GetRelativePathFromCWD(initialCWD, options.ManifestPath)
				if err != nil {
					return err
				}
				// as the installer uses root for executing the pipeline, we save the rel path from root as ManifestPathFlag option
				options.ManifestPathFlag = manifestPathFlag

				// when the manifest path is set by the cmd flag, we are moving cwd so the cmd is executed from that dir
				uptManifestPath, err := model.UpdateCWDtoManifestPath(options.ManifestPath)
				if err != nil {
					return err
				}
				options.ManifestPath = uptManifestPath
			}
			if err := contextCMD.LoadContextFromPath(ctx, options.Namespace, options.K8sContext, options.ManifestPath); err != nil {
				if err.Error() == fmt.Errorf(oktetoErrors.ErrNotLogged, okteto.CloudURL).Error() {
					return err
				}
				if err := contextCMD.NewContextCommand().Run(ctx, &contextCMD.ContextOptions{Namespace: options.Namespace}); err != nil {
					return err
				}
			}

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to get the current working directory: %w", err)
			}
			name := options.Name
			if options.Name == "" {
				c, _, err := okteto.NewK8sClientProvider().Provide(okteto.Context().Cfg)
				if err != nil {
					return err
				}
				inferer := devenvironment.NewNameInferer(c)
				name = inferer.InferName(ctx, cwd, okteto.Context().Namespace, options.ManifestPathFlag)
				if err != nil {
					return fmt.Errorf("could not infer environment name")
				}
			}

			dynClient, _, err := okteto.GetDynamicClient()
			if err != nil {
				return err
			}
			discClient, _, err := okteto.GetDiscoveryClient()
			if err != nil {
				return err
			}
			k8sClient, cfg, err := okteto.GetK8sClient()
			if err != nil {
				return err
			}

			if options.Namespace == "" {
				options.Namespace = okteto.Context().Namespace
			}

			okClient, err := okteto.NewOktetoClient()
			if err != nil {
				return err
			}

			c := &destroyCommand{
				executor:          executor.NewExecutor(oktetoLog.GetOutputFormat(), options.RunWithoutBash),
				ConfigMapHandler:  NewConfigmapHandler(k8sClient),
				nsDestroyer:       namespaces.NewNamespace(dynClient, discClient, cfg, k8sClient),
				secrets:           secrets.NewSecrets(k8sClient),
				k8sClientProvider: okteto.NewK8sClientProvider(),
				oktetoClient:      okClient,
			}

			kubeconfigPath := getTempKubeConfigFile(name)
			if err := kubeconfig.Write(okteto.Context().Cfg, kubeconfigPath); err != nil {
				return err
			}
			os.Setenv("KUBECONFIG", kubeconfigPath)
			defer os.Remove(kubeconfigPath)

			destroyer, err := c.getDestroyer(options)
			if err != nil {
				return err
			}

			return destroyer.destroy(ctx, options)
		},
	}

	cmd.Flags().StringVar(&options.Name, "name", "", "development environment name")
	cmd.Flags().StringVarP(&options.ManifestPath, "file", "f", "", "path to the manifest file")
	cmd.Flags().BoolVarP(&options.DestroyVolumes, "volumes", "v", false, "remove persistent volumes")
	cmd.Flags().BoolVarP(&options.DestroyDependencies, "dependencies", "d", false, "destroy dependencies")
	cmd.Flags().BoolVar(&options.ForceDestroy, "force-destroy", false, "forces the development environment to be destroyed even if there is an error executing the custom destroy commands defined in the manifest")
	cmd.Flags().StringVarP(&options.Namespace, "namespace", "n", "", "overwrites the namespace where the development environment was deployed")
	cmd.Flags().StringVarP(&options.K8sContext, "context", "c", "", "context where the development environment was deployed")
	cmd.Flags().BoolVarP(&options.RunWithoutBash, "no-bash", "", false, "execute commands without bash")
	cmd.Flags().BoolVarP(&options.DestroyAll, "all", "", false, "destroy everything in the namespace")
	cmd.Flags().BoolVarP(&options.RunInRemote, "remote", "", false, "force run destroy commands in remote")

	return cmd
}

func getTempKubeConfigFile(name string) string {
	tempKubeconfigFileName := fmt.Sprintf("kubeconfig-destroy-%s-%d", name, time.Now().UnixMilli())
	return filepath.Join(config.GetOktetoHome(), tempKubeconfigFileName)
}

func (dc *destroyCommand) getDestroyer(opts *Options) (destroyInterface, error) {
	var (
		deployer destroyInterface
		err      error
	)

	if opts.DestroyAll {
		if !okteto.Context().IsOkteto {
			return nil, oktetoErrors.ErrContextIsNotOktetoCluster
		}
		deployer, err = newLocalDestroyerAll(dc.k8sClientProvider, dc.executor, dc.nsDestroyer, dc.oktetoClient)
		if err != nil {
			return nil, err
		}

		oktetoLog.Info("Destroying all...")
	} else {

		manifest, err := model.GetManifestV2(opts.ManifestPath)
		if err != nil {
			// Log error message but application can still be deleted
			oktetoLog.Infof("could not find manifest file to be executed: %s", err)
			manifest = &model.Manifest{
				Destroy: &model.DestroyInfo{},
			}
		}

		isRemote := utils.LoadBoolean(constants.OKtetoDeployRemote)

		destroyImage := ""
		if manifest.Destroy != nil {
			destroyImage = manifest.Destroy.Image
		}
		runInRemote := !isRemote && (destroyImage != "" || opts.RunInRemote)

		if runInRemote {
			deployer = newRemoteDestroyer(manifest)
			oktetoLog.Info("Destroying remotely...")
		} else {
			destroyerAll, err := newLocalDestroyerAll(dc.k8sClientProvider, dc.executor, dc.nsDestroyer, dc.oktetoClient)
			if err != nil {
				return nil, err
			}

			deployer = newLocalDestroyer(manifest, destroyerAll)
			oktetoLog.Info("Destroying locally...")
		}
	}

	return deployer, nil
}
