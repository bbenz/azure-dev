package provisioning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/azure/azure-dev/cli/azd/pkg/environment"
	"github.com/azure/azure-dev/cli/azd/pkg/infra"
	"github.com/azure/azure-dev/cli/azd/pkg/tools"
	"github.com/drone/envsubst"
)

type BicepTemplate struct {
	Schema         string                    `json:"$schema`
	ContentVersion string                    `json:"contentVersion"`
	Parameters     map[string]BicepParameter `json:"parameters"`
}

type BicepParameter struct {
	Type         string      `json:"type"`
	DefaultValue interface{} `json:"defaultValue"`
	Value        interface{} `json:"value"`
}

// BicepInfraProvider exposes infrastructure provisioning using Azure Bicep templates
type BicepInfraProvider struct {
	env         *environment.Environment
	projectPath string
	options     InfrastructureOptions
	bicep       tools.BicepCli
	az          tools.AzCli
}

// Name gets the name of the infra provider
func (p *BicepInfraProvider) Name() string {
	return "Bicep"
}

// Compiles the specified template
func (p *BicepInfraProvider) Compile(ctx context.Context) (*CompiledTemplate, error) {
	bicepTemplate, err := p.createParametersFile()
	if err != nil {
		return nil, fmt.Errorf("creating parameters file: %w", err)
	}

	modulePath := p.modulePath()
	template, err := p.createTemplate(ctx, modulePath)
	if err != nil {
		return nil, fmt.Errorf("creating template: %w", err)
	}

	// Merge parameter values from template
	for key, param := range template.Parameters {
		if bicepParam, has := bicepTemplate.Parameters[key]; has {
			param.Value = bicepParam.Value
			template.Parameters[key] = param
		}
	}

	return template, nil
}

func (p *BicepInfraProvider) SaveTemplate(ctx context.Context, template CompiledTemplate) error {
	bicepFile := BicepTemplate{
		Schema:         "https://schema.management.azure.com/schemas/2019-04-01/deploymentParameters.json#",
		ContentVersion: "1.0.0.0",
	}

	parameters := make(map[string]BicepParameter)

	for key, param := range template.Parameters {
		parameters[key] = BicepParameter{
			Type:         param.Type,
			DefaultValue: param.DefaultValue,
			Value:        param.Value,
		}
	}

	bicepFile.Parameters = parameters

	bytes, err := json.MarshalIndent(bicepFile, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling parameters: %w", err)
	}

	parametersFilePath := p.parametersFilePath()
	err = ioutil.WriteFile(parametersFilePath, bytes, 0644)
	if err != nil {
		return fmt.Errorf("writing parameters file: %w", err)
	}

	return nil
}

// Provisioning the infrastructure within the specified template
func (p *BicepInfraProvider) Deploy(ctx context.Context, scope ProvisioningScope) (<-chan *InfraDeploymentResult, <-chan *InfraDeploymentProgress) {
	result := make(chan *InfraDeploymentResult, 1)
	progress := make(chan *InfraDeploymentProgress)

	go func() {
		defer close(result)
		defer close(progress)

		// Do the deployment
		go func() {
			// Do the creating. The call to `DeployToSubscription` blocks until the deployment completes,
			// which can take a bit, so we typically do some progress indication.
			// For interactive use (default case, using table formatter), we use a spinner.
			// With JSON formatter we emit progress information, unless --no-progress option was set.
			modulePath := p.modulePath()
			parametersFilePath := p.parametersFilePath()

			deployResult, err := p.deployModule(ctx, scope, modulePath, parametersFilePath)
			result <- &InfraDeploymentResult{
				Error:      err,
				Operations: nil,
				Outputs:    p.createOutputParameters((*deployResult).Properties.Outputs),
			}
		}()

		for {
			resourceManager := infra.NewAzureResourceManager(p.az)

			ops, err := resourceManager.GetDeploymentResourceOperations(ctx, p.env.GetSubscriptionId(), p.env.GetEnvName())
			if err != nil || len(*ops) == 0 {
				// Status display is best-effort activity.
				return
			}

			progressReport := InfraDeploymentProgress{
				Timestamp:  time.Now(),
				Operations: *ops,
			}

			progress <- &progressReport
		}
	}()

	return result, progress
}

func (p *BicepInfraProvider) createOutputParameters(azureOutputParams map[string]tools.AzCliDeploymentOutput) []InfraDeploymentOutputParameter {
	outputParams := []InfraDeploymentOutputParameter{}

	for key, param := range azureOutputParams {
		outputParams = append(outputParams, InfraDeploymentOutputParameter{
			Name:  key,
			Type:  param.Type,
			Value: param.Value,
		})
	}

	return outputParams
}

// Copies the Bicep parameters file from the project template into the .azure environment folder
func (p *BicepInfraProvider) createParametersFile() (*BicepTemplate, error) {
	// Copy the parameter template file to the environment working directory and do substitutions.
	parametersTemplateFilePath := p.parametersTemplateFilePath()
	log.Printf("Reading parameters template file from: %s", parametersTemplateFilePath)
	parametersBytes, err := ioutil.ReadFile(parametersTemplateFilePath)
	if err != nil {
		return nil, fmt.Errorf("reading parameter file template: %w", err)
	}
	replaced, err := envsubst.Eval(string(parametersBytes), func(name string) string {
		if val, has := p.env.Values[name]; has {
			return val
		}
		return os.Getenv(name)
	})

	if err != nil {
		return nil, fmt.Errorf("substituting parameter file: %w", err)
	}

	parametersFilePath := p.parametersFilePath()
	writeDir := filepath.Dir(parametersFilePath)
	if err := os.MkdirAll(writeDir, 0755); err != nil {
		return nil, fmt.Errorf("creating directory structure: %w", err)
	}

	log.Printf("Writing parameters file to: %s", parametersFilePath)
	err = ioutil.WriteFile(parametersFilePath, []byte(replaced), 0644)
	if err != nil {
		return nil, fmt.Errorf("writing parameter file: %w", err)
	}

	var bicepTemplate BicepTemplate
	if err := json.Unmarshal([]byte(replaced), &bicepTemplate); err != nil {
		return nil, fmt.Errorf("error unmarshalling Bicep template parameters: %w", err)
	}

	return &bicepTemplate, nil
}

// Creates the compiled template from the specified module path
func (p *BicepInfraProvider) createTemplate(ctx context.Context, modulePath string) (*CompiledTemplate, error) {
	// Compile the bicep file into an ARM template we can create.
	compiled, err := p.bicep.Build(ctx, modulePath)
	if err != nil {
		return nil, fmt.Errorf("failed to compile bicep template: %w", err)
	}

	// Fetch the parameters from the template and ensure we have a value for each one, otherwise
	// prompt.
	var bicepTemplate BicepTemplate
	if err := json.Unmarshal([]byte(compiled), &bicepTemplate); err != nil {
		log.Printf("failed un-marshaling compiled arm template to JSON (err: %v), template contents:\n%s", err, compiled)
		return nil, fmt.Errorf("error un-marshaling arm template from json: %w", err)
	}

	compiledTemplate, err := p.convertToCompiledTemplate(bicepTemplate)
	if err != nil {
		return nil, fmt.Errorf("converting from bicep to compiled template: %w", err)
	}

	return compiledTemplate, nil
}

// Converts a Bicep parameters file to a generic provisioning template
func (p *BicepInfraProvider) convertToCompiledTemplate(bicepParamsFile BicepTemplate) (*CompiledTemplate, error) {
	template := CompiledTemplate{}
	parameters := make(map[string]CompiledTemplateParameter)

	for key, param := range bicepParamsFile.Parameters {
		parameters[key] = CompiledTemplateParameter{
			Type:         param.Type,
			Value:        param.Value,
			DefaultValue: param.DefaultValue,
		}
	}

	template.Parameters = parameters

	return &template, nil
}

// Deploys the specified Bicep module and parameters with the selected provisioning scope (subscription vs resource group)
func (p *BicepInfraProvider) deployModule(ctx context.Context, scope ProvisioningScope, bicepPath string, parametersPath string) (*tools.AzCliDeployment, error) {
	// We've seen issues where `Deploy` completes but for a short while after, fetching the deployment fails with a `DeploymentNotFound` error.
	// Since other commands of ours use the deployment, let's try to fetch it here and if we fail with `DeploymentNotFound`,
	// ignore this error, wait a short while and retry.
	if err := scope.Deploy(ctx, bicepPath, parametersPath); err != nil {
		return nil, fmt.Errorf("failed deploying: %w", err)
	}

	var deployment tools.AzCliDeployment
	var err error

	for i := 0; i < 10; i++ {
		time.Sleep(time.Duration(math.Min(float64(i), 3)*10) * time.Second)
		deployment, err = scope.GetDeployment(ctx)
		if errors.Is(err, tools.ErrDeploymentNotFound) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("failed waiting for deployment: %w", err)
		} else {
			return &deployment, nil
		}
	}

	return nil, fmt.Errorf("timed out waiting for deployment: %w", err)
}

// Gets the path to the project parameters file path
func (p *BicepInfraProvider) parametersTemplateFilePath() string {
	infraPath := p.options.Path
	if strings.TrimSpace(infraPath) == "" {
		infraPath = "infra"
	}

	parametersFilename := fmt.Sprintf("%s.parameters.json", p.options.Module)
	return filepath.Join(p.projectPath, infraPath, parametersFilename)
}

// Gets the path to the staging .azure parameters file path
func (p *BicepInfraProvider) parametersFilePath() string {
	parametersFilename := fmt.Sprintf("%s.parameters.json", p.options.Module)
	return filepath.Join(p.projectPath, ".azure", p.env.GetEnvName(), p.options.Path, parametersFilename)
}

// Gets the folder path to the specified module
func (p *BicepInfraProvider) modulePath() string {
	infraPath := p.options.Path
	if strings.TrimSpace(infraPath) == "" {
		infraPath = "infra"
	}

	moduleFilename := fmt.Sprintf("%s.bicep", p.options.Module)
	return filepath.Join(p.projectPath, infraPath, moduleFilename)
}

// NewBicepInfraProvider creates a new instance of a Bicep Infra provider
func NewBicepInfraProvider(env *environment.Environment, projectPath string, options InfrastructureOptions, bicep tools.BicepCli, az tools.AzCli) InfraProvider {
	return &BicepInfraProvider{
		env:         env,
		projectPath: projectPath,
		options:     options,
		bicep:       bicep,
		az:          az,
	}
}
