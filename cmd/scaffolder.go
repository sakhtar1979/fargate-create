package cmd

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	getter "github.com/hashicorp/go-getter"
	"gopkg.in/yaml.v2"
)

type scaffoldTemplate struct {
	Base templateDirectory
	Env  templateDirectory
}

type templateDirectory struct {
	Directory     string
	Configuration *templateConfig
	Installed     bool
}

type templateConfig struct {
	Prompts []*prompt `yaml:"prompts"`
}

type prompt struct {
	Question          string   `yaml:"question"`
	Default           string   `yaml:"default"`
	FilesToDeleteIfNo []string `yaml:"filesToDeleteIfNo"`
}

func scaffold(context *scaffoldContext) {

	//scaffold out infrastructure files
	template := scaffoldInfrastructure(context)

	//scaffold application files
	scaffoldApplication(context, template)

	//apply any template configurations
	applyTemplateConfiguration(template.Base)
	applyTemplateConfiguration(template.Env)
}

func applyTemplateConfiguration(t templateDirectory) {
	if t.Configuration != nil {
		for _, prompt := range t.Configuration.Prompts {
			//if -y, use defaults, otherwise prompt
			response := prompt.Default
			if !yesUseDefaults {
				fmt.Println()
				q := fmt.Sprintf("%s (%s) ", prompt.Question, prompt.Default)
				response = promptAndGetResponse(q, prompt.Default)
			}
			yes := containsString(okayResponses, response)
			if !yes && prompt.FilesToDeleteIfNo != nil {
				for _, file := range prompt.FilesToDeleteIfNo {
					p := filepath.Join(t.Directory, file)
					fmt.Println("deleting ", p)
					err := os.Remove(p)
					check(err)
				}
			}
		}
	}
}

func scaffoldInfrastructure(context *scaffoldContext) *scaffoldTemplate {

	//fetch terraform template
	templateDir := downloadTerraformTemplate()
	debug("downloaded to:", templateDir)

	result := installTerraformTemplate(templateDir, context.Env)
	debug("environment installed to:", result.Env.Directory)

	//copy var file into base module
	if result.Base.Installed {
		debug(fmt.Sprintf("copying %s to %s", varFile, result.Base.Directory))
		targetFile := getTargetVarFile(context.Format)
		err := copyFile(varFile, filepath.Join(result.Base.Directory, targetFile))
		check(err)
	}

	//copy var file into environment module
	debug(fmt.Sprintf("copying %s to %s", varFile, result.Env.Directory))
	targetFile := getTargetVarFile(context.Format)
	err := copyFile(varFile, filepath.Join(result.Env.Directory, targetFile))
	check(err)

	//update tf backend in main.tf to match app/env
	mainTfFile := filepath.Join(result.Env.Directory, "main.tf")
	fileBits, err := ioutil.ReadFile(mainTfFile)
	check(err)
	maintf := updateTerraformBackend(string(fileBits), context.Profile, context.App, context.Env)
	err = ioutil.WriteFile(mainTfFile, []byte(maintf), 0644)
	check(err)

	return result
}

//fetches and installs the tf template and returns the output directory
func downloadTerraformTemplate() string {

	client := getter.Client{
		Src:  templateURL,
		Dst:  "./" + tempDir,
		Mode: getter.ClientModeAny,
	}

	fmt.Println("downloading terraform template", templateURL)
	err := client.Get()
	check(err)
	debug("done")

	return client.Dst
}

//installs a template for the specified environment and returns a scaffoldTemplate
func installTerraformTemplate(templateDir string, environment string) *scaffoldTemplate {

	result := scaffoldTemplate{
		Base: templateDirectory{},
		Env:  templateDirectory{},
	}

	//create infrastructure directory (if not already there)
	targetInfraDir := targetDir
	fmt.Println("installing terraform template")
	if _, err := os.Stat(targetInfraDir); os.IsNotExist(err) {
		debug("creating directory:", targetInfraDir)
		err = os.MkdirAll(targetInfraDir, 0755)
		check(err)
	} else {
		debug(targetInfraDir + " already exists")
	}

	//copy over infrastructure/base (if not already there)
	baseDir := "base"
	sourceBaseDir := filepath.Join(templateDir, baseDir)
	destBaseDir := filepath.Join(targetInfraDir, baseDir)
	if _, err := os.Stat(destBaseDir); os.IsNotExist(err) {
		debug(fmt.Sprintf("copying %s to %s", sourceBaseDir, destBaseDir))
		err = copyDir(sourceBaseDir, destBaseDir)
		check(err)

		result.Base.Installed = true
		result.Base.Directory = destBaseDir

		//does template contain a fargate-create.yml config?  is so, load it
		result.Base.Configuration = loadTemplateConfig(result.Base.Directory)

	} else {
		fmt.Println(destBaseDir + " already exists, ignoring")
	}

	//if environment directory exists, prompt to override, if no, then exit
	sourceEnvDir := filepath.Join(templateDir, "env", "dev")
	destEnvDir := filepath.Join(targetInfraDir, "env", environment)

	yes := true
	if _, err := os.Stat(destEnvDir); err == nil {
		//exists
		fmt.Print(destEnvDir + " already exists. Overwrite? ")
		if yes = askForConfirmation(); yes {
			debug("deleting", destEnvDir)
			//delete environment directory (all files)
			err = os.RemoveAll(destEnvDir)
			check(err)
		}
	} else {
		//doesn't exist
		debug(destEnvDir + " doesn't exist")
	}

	if yes {
		//env directory either doesn't exist or user wants to overwrite
		//copy repo/env/${env} -> ./infrastructure/env/${env}
		debug(fmt.Sprintf("copying %s to %s", sourceEnvDir, destEnvDir))
		err := copyDir(sourceEnvDir, destEnvDir)
		check(err)

		result.Env.Installed = true
		result.Env.Directory = destEnvDir

		//does template contain a fargate-create.yml config?  is so, load it
		result.Env.Configuration = loadTemplateConfig(result.Env.Directory)
	}

	// finally, delete temp dir
	debug("deleting:", tempDir)
	err := os.RemoveAll(tempDir)
	check(err)

	return &result
}

func loadTemplateConfig(dir string) *templateConfig {
	configFile := filepath.Join(dir, templateConfigFile)
	var config templateConfig
	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		debug("found template config: ", dir)
		//load yaml
		dat, err := ioutil.ReadFile(configFile)
		check(err)
		err = yaml.Unmarshal(dat, &config)
		check(err)
	} else {
		debug("didn't find template config: ", dir)
	}
	return &config
}

func getTargetVarFile(format string) string {
	targetFile := ""
	if format == varFormatHCL {
		targetFile = "terraform.tfvars"
	}
	if format == varFormatJSON {
		targetFile = "terraform.tfvars.json"
	}
	return targetFile
}

func scaffoldApplication(context *scaffoldContext, t *scaffoldTemplate) {

	//write the application files to the env directory
	targetAppDir := t.Env.Directory

	//write a docker-compose.yml file
	dockerComposeYml := getDockerComposeYml(context)
	dockerComposeYmlFile := filepath.Join(targetAppDir, "docker-compose.yml")
	debug("writing", dockerComposeYmlFile)
	err := ioutil.WriteFile(dockerComposeYmlFile, []byte(dockerComposeYml), 0644)
	check(err)

	//write hidden.env
	hiddenEnvFileName := filepath.Join(targetAppDir, "hidden.env")
	sampleContents := "#FOO=bar\n"
	err = ioutil.WriteFile(hiddenEnvFileName, []byte(sampleContents), 0644)
	check(err)

	//write a fargate.yml for the cli
	fargateYml := getFargateYaml(context)
	fargateYmlFile := filepath.Join(targetAppDir, "fargate.yml")
	debug("writing", fargateYmlFile)
	err = ioutil.WriteFile(fargateYmlFile, []byte(fargateYml), 0644)
	check(err)

	//write deploy.sh
	deployScript := getDeployScript(context)
	deployScriptFile := filepath.Join(targetAppDir, "deploy.sh")
	debug("writing", deployScriptFile)
	err = ioutil.WriteFile(deployScriptFile, []byte(deployScript), 0755)
	check(err)

	//ignored files
	hiddenenv := strings.Split(hiddenEnvFileName, "/")
	ignoredFiles := []string{hiddenenv[len(hiddenenv)-1], ".terraform"}
	ensureFileContains(".gitignore", ignoredFiles)
	ensureFileContains(".dockerignore", ignoredFiles)
}

func getFargateYaml(context *scaffoldContext) string {
	textTemplate := `cluster: {{.App}}-{{.Env}}
service: {{.App}}-{{.Env}}
`
	return applyTemplate(textTemplate, context)
}

func getDockerComposeYml(context *scaffoldContext) string {
	t := `version: "3.4"
services:
	{{.App}}:
		build: ../../../
		image: {{.AccountID}}.dkr.ecr.{{.Region}}.amazonaws.com/{{.App}}:0.1.0
		ports:    
		- 80:8080
		env_file:
		- hidden.env	
`
	return applyTemplate(t, context)
}

func getDeployScript(context *scaffoldContext) string {
	t := `#! /bin/bash
set -e

# build image
docker-compose build

# push image to ECR repo
export AWS_PROFILE={{.Profile}}
export AWS_DEFAULT_REGION={{.Region}}
login=$(aws ecr get-login --no-include-email) && eval "$login"
docker-compose push

# deploy image and env vars
fargate service deploy -f docker-compose.yml
`
	return applyTemplate(t, context)
}
