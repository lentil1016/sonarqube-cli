package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sqclient "github.com/tektoncd/operator/tools/sonarqube-cli/pkg/client"
	"github.com/tektoncd/operator/tools/sonarqube-cli/pkg/config"
	"gopkg.in/yaml.v3"
)

func TestRunCreate_RequiresTokenFileBeforeAPICalls(t *testing.T) {
	oldConfigFile := configFile
	oldPlugin := plugin
	oldTokenFile := tokenFile
	oldStateFile := stateFile
	t.Cleanup(func() {
		configFile = oldConfigFile
		plugin = oldPlugin
		tokenFile = oldTokenFile
		stateFile = oldStateFile
	})

	configFile = "does-not-matter.yaml"
	plugin = "tektoncd"
	tokenFile = ""
	stateFile = "does-not-matter.state"

	err := runCreate(createCmd, nil)
	if err == nil {
		t.Fatal("runCreate() error = nil, want token-file validation error")
	}
	if !strings.Contains(err.Error(), "--token-file is required") {
		t.Fatalf("runCreate() error = %v, want token-file validation error", err)
	}
}

func TestRunCreate_RejectsExistingStateFileBeforeLoadingConfig(t *testing.T) {
	dir := t.TempDir()
	existingStateFile := filepath.Join(dir, "resource.state")
	if err := os.WriteFile(existingStateFile, []byte("existing-state"), 0600); err != nil {
		t.Fatalf("WriteFile(state) error = %v", err)
	}

	oldConfigFile := configFile
	oldPlugin := plugin
	oldTokenFile := tokenFile
	oldStateFile := stateFile
	oldResolvedConfig := resolvedConfig
	oldOutputTemplate := outputTemplate
	oldOutputFile := outputFile
	t.Cleanup(func() {
		configFile = oldConfigFile
		plugin = oldPlugin
		tokenFile = oldTokenFile
		stateFile = oldStateFile
		resolvedConfig = oldResolvedConfig
		outputTemplate = oldOutputTemplate
		outputFile = oldOutputFile
	})

	configFile = filepath.Join(dir, "does-not-matter.yaml")
	plugin = "tektoncd"
	tokenFile = filepath.Join(dir, "token.env")
	stateFile = existingStateFile
	resolvedConfig = ""
	outputTemplate = ""
	outputFile = ""

	err := runCreate(createCmd, nil)
	if err == nil {
		t.Fatal("runCreate() error = nil, want existing state-file validation error")
	}
	if !strings.Contains(err.Error(), "--state-file already exists") {
		t.Fatalf("runCreate() error = %v, want existing state-file validation error", err)
	}
}

func TestCreateCommand_RequiresTokenFileFlag(t *testing.T) {
	flag := createCmd.Flag("token-file")
	if flag == nil {
		t.Fatal("createCmd token-file flag = nil")
	}
	if _, ok := createCmd.Flag("token-file").Annotations["cobra_annotation_bash_completion_one_required_flag"]; !ok {
		t.Fatal("createCmd token-file flag is not marked required")
	}
	if _, ok := createCmd.Flag("state-file").Annotations["cobra_annotation_bash_completion_one_required_flag"]; !ok {
		t.Fatal("createCmd state-file flag is not marked required")
	}
	if _, ok := cleanupCmd.Flag("state-file").Annotations["cobra_annotation_bash_completion_one_required_flag"]; !ok {
		t.Fatal("cleanupCmd state-file flag is not marked required")
	}
}

func TestFilterMatchesTemplatedPluginName(t *testing.T) {
	res := config.TempResource{
		PluginName: "${PLUGIN_NAME}",
	}

	resolvedPluginName, err := config.ReplaceVariables(res.PluginName, "task-1", "tektoncd")
	if err != nil {
		t.Fatalf("ReplaceVariables() error = %v", err)
	}
	if resolvedPluginName != "tektoncd" {
		t.Fatalf("ReplaceVariables() = %q, want %q", resolvedPluginName, "tektoncd")
	}
}

func TestValidateCreateOutputTargets_RejectsExistingPaths(t *testing.T) {
	dir := t.TempDir()

	testCases := []struct {
		name string
		flag string
		path string
	}{
		{name: "token file", flag: "--token-file", path: filepath.Join(dir, "token.env")},
		{name: "resolved config", flag: "--resolved-config", path: filepath.Join(dir, "resolved.yaml")},
		{name: "output file", flag: "--output-file", path: filepath.Join(dir, "output.txt")},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(tc.path, []byte("existing"), 0600); err != nil {
				t.Fatalf("WriteFile(%s) error = %v", tc.name, err)
			}

			err := validateCreateOutputTargets(outputTarget{flag: tc.flag, path: tc.path})
			if err == nil {
				t.Fatalf("validateCreateOutputTargets() error = nil, want existing %s validation error", tc.flag)
			}
			if !strings.Contains(err.Error(), tc.flag+" already exists") {
				t.Fatalf("validateCreateOutputTargets() error = %v, want %s already exists", err, tc.flag)
			}
		})
	}
}

func TestValidateCreateOutputTargets_RejectsDuplicatePaths(t *testing.T) {
	dir := t.TempDir()
	sharedPath := filepath.Join(dir, "shared.out")

	err := validateCreateOutputTargets(
		outputTarget{flag: "--state-file", path: sharedPath},
		outputTarget{flag: "--token-file", path: sharedPath},
	)
	if err == nil {
		t.Fatal("validateCreateOutputTargets() error = nil, want duplicate path validation error")
	}
	if !strings.Contains(err.Error(), "--token-file path") || !strings.Contains(err.Error(), "--state-file") {
		t.Fatalf("validateCreateOutputTargets() error = %v, want duplicate path details", err)
	}
}

func TestResolveTaskRunID_PrefersFlagOverEnv(t *testing.T) {
	t.Setenv("TASK_RUN_ID", "env-task")

	if got := resolveTaskRunID("flag-task"); got != "flag-task" {
		t.Fatalf("resolveTaskRunID() = %q, want %q", got, "flag-task")
	}
}

func TestResolveTaskRunID_UsesEnvWhenFlagEmpty(t *testing.T) {
	t.Setenv("TASK_RUN_ID", "env-task")

	if got := resolveTaskRunID(""); got != "env-task" {
		t.Fatalf("resolveTaskRunID() = %q, want %q", got, "env-task")
	}
}

// TestParseOutputEndpoint_UsesExplicitPort verifies explicit endpoint ports are preserved for template rendering.
func TestParseOutputEndpoint_UsesExplicitPort(t *testing.T) {
	host, port, scheme, err := parseOutputEndpoint("http://sonarqube.example.com:9000")
	if err != nil {
		t.Fatalf("parseOutputEndpoint() error = %v", err)
	}
	if host != "sonarqube.example.com" {
		t.Fatalf("parseOutputEndpoint() host = %q, want sonarqube.example.com", host)
	}
	if port != 9000 {
		t.Fatalf("parseOutputEndpoint() port = %d, want 9000", port)
	}
	if scheme != "http" {
		t.Fatalf("parseOutputEndpoint() scheme = %q, want http", scheme)
	}
}

// TestParseOutputEndpoint_UsesDefaultHTTPSPort verifies HTTPS endpoints without an explicit port still render a usable port.
func TestParseOutputEndpoint_UsesDefaultHTTPSPort(t *testing.T) {
	host, port, scheme, err := parseOutputEndpoint("https://sonarqube.example.com")
	if err != nil {
		t.Fatalf("parseOutputEndpoint() error = %v", err)
	}
	if host != "sonarqube.example.com" {
		t.Fatalf("parseOutputEndpoint() host = %q, want sonarqube.example.com", host)
	}
	if port != 443 {
		t.Fatalf("parseOutputEndpoint() port = %d, want 443", port)
	}
	if scheme != "https" {
		t.Fatalf("parseOutputEndpoint() scheme = %q, want https", scheme)
	}
}

// TestRunCreate_OutputTemplateIncludesDerivedEndpointFields verifies output templates receive Host, Port, and Scheme.
func TestRunCreate_OutputTemplateIncludesDerivedEndpointFields(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	templatePath := filepath.Join(dir, "output.tmpl")
	outputPath := filepath.Join(dir, "output.yaml")
	tokenPath := filepath.Join(dir, "token.env")
	statePath := filepath.Join(dir, "state.yaml")

	oldConfigFile := configFile
	oldTaskRunID := taskRunID
	oldPlugin := plugin
	oldOutputTemplate := outputTemplate
	oldOutputFile := outputFile
	oldTokenFile := tokenFile
	oldStateFile := stateFile
	oldManagerTokenFile := managerTokenFile
	oldTempUserPasswordFile := tempUserPasswordFile
	t.Cleanup(func() {
		configFile = oldConfigFile
		taskRunID = oldTaskRunID
		plugin = oldPlugin
		outputTemplate = oldOutputTemplate
		outputFile = oldOutputFile
		tokenFile = oldTokenFile
		stateFile = oldStateFile
		managerTokenFile = oldManagerTokenFile
		tempUserPasswordFile = oldTempUserPasswordFile
	})

	templateData := `endpoint={{ $.Endpoint }}
host={{ $.Host }}
port={{ $.Port }}
scheme={{ $.Scheme }}
user={{ (index .Users 0).Login }}
project={{ (index .Projects 0).Key }}
`
	if err := os.WriteFile(templatePath, []byte(templateData), 0600); err != nil {
		t.Fatalf("WriteFile(template) error = %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/authorizations/groups", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/api/users/create", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/user_groups/add_user", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/permissions/create_template", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/permissions/add_group_to_template", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/projects/create", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/user_tokens/generate", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"token":"generated-token"}`)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	cfgData := fmt.Sprintf(`sonarqube:
  endpoint: %s
  manager:
    username: manager
    token: manager-token
  temp_resources:
    - plugin_name: tektoncd
      group:
        name: test-group
        description: Temporary group
      user:
        login: test-user
        name: Test User
        email: test@example.com
        password: password
      projects:
        - key: test-project
          name: Test Project
      permission_template:
        name: test-template
        description: Test Template
        project_key_pattern: test-.*
        permissions:
          - user
`, server.URL)
	if err := os.WriteFile(cfgPath, []byte(cfgData), 0600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	configFile = cfgPath
	taskRunID = ""
	plugin = "tektoncd"
	outputTemplate = templatePath
	outputFile = outputPath
	tokenFile = tokenPath
	stateFile = statePath
	managerTokenFile = ""
	tempUserPasswordFile = ""

	t.Setenv("SONARQUBE_ALLOW_HTTP", "true")
	if err := runCreate(createCmd, nil); err != nil {
		t.Fatalf("runCreate() error = %v", err)
	}

	parsedURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}

	outputData, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile(output) error = %v", err)
	}
	output := string(outputData)
	if !strings.Contains(output, "endpoint="+server.URL) {
		t.Fatalf("output template missing endpoint: %s", output)
	}
	if !strings.Contains(output, "host="+parsedURL.Hostname()) {
		t.Fatalf("output template missing host %q: %s", parsedURL.Hostname(), output)
	}
	if !strings.Contains(output, "port="+parsedURL.Port()) {
		t.Fatalf("output template missing port %q: %s", parsedURL.Port(), output)
	}
	if !strings.Contains(output, "scheme="+parsedURL.Scheme) {
		t.Fatalf("output template missing scheme %q: %s", parsedURL.Scheme, output)
	}
	if !strings.Contains(output, "user=test-user") {
		t.Fatalf("output template missing user: %s", output)
	}
	if !strings.Contains(output, "project=test-project") {
		t.Fatalf("output template missing project: %s", output)
	}
}

func TestResolveTempResourceForOutput_ReplacesTemplates(t *testing.T) {
	res := resolveTempResourceForOutput(config.TempResource{
		PluginName: "${PLUGIN_NAME}",
		Group: config.Group{
			Name:        "group-${TASK_RUN_ID}",
			Description: "group for ${PLUGIN_NAME}",
		},
		User: config.User{
			Login:    "user-${TASK_RUN_ID}",
			Name:     "User ${PLUGIN_NAME}",
			Email:    "user-${TASK_RUN_ID}@example.com",
			Password: "${TEMP_USER_PASSWORD}",
			Groups:   []string{"${PLUGIN_NAME}-group"},
		},
		Projects: []config.Project{
			{Key: "${PLUGIN_NAME}-${TASK_RUN_ID}", Name: "Project ${PLUGIN_NAME}"},
		},
		PermissionTemplate: config.PermissionTemplate{
			Name:              "template-${TASK_RUN_ID}",
			Description:       "Template ${PLUGIN_NAME}",
			ProjectKeyPattern: "${PLUGIN_NAME}-${TASK_RUN_ID}.*",
		},
	}, "task-1", "tektoncd", true)

	if res.PluginName != "tektoncd" {
		t.Fatalf("PluginName = %q, want tektoncd", res.PluginName)
	}
	if res.Group.Name != "group-task-1" {
		t.Fatalf("Group.Name = %q, want group-task-1", res.Group.Name)
	}
	if res.User.Login != "user-task-1" {
		t.Fatalf("User.Login = %q, want user-task-1", res.User.Login)
	}
	if res.Projects[0].Key != "tektoncd-task-1" {
		t.Fatalf("Projects[0].Key = %q, want tektoncd-task-1", res.Projects[0].Key)
	}
	if res.PermissionTemplate.ProjectKeyPattern != "tektoncd-task-1.*" {
		t.Fatalf("PermissionTemplate.ProjectKeyPattern = %q, want tektoncd-task-1.*", res.PermissionTemplate.ProjectKeyPattern)
	}
	if res.User.Password != "${TEMP_USER_PASSWORD}" {
		t.Fatalf("User.Password = %q, want secret placeholder preserved", res.User.Password)
	}
}

func TestResolveTempResourceForOutput_PreservesUnknownPluginTemplates(t *testing.T) {
	res := resolveTempResourceForOutput(config.TempResource{
		PluginName: "${PLUGIN_NAME}",
		Group: config.Group{
			Name: "group-${PLUGIN_NAME}-${TASK_RUN_ID}",
		},
		User: config.User{
			Login: "user-${PLUGIN_NAME}-${TASK_RUN_ID}",
		},
		Projects: []config.Project{
			{Key: "${PLUGIN_NAME}-${TASK_RUN_ID}"},
		},
	}, "task-1", "tektoncd", false)

	if res.PluginName != "${PLUGIN_NAME}" {
		t.Fatalf("PluginName = %q, want placeholder preserved", res.PluginName)
	}
	if res.Group.Name != "group-${PLUGIN_NAME}-task-1" {
		t.Fatalf("Group.Name = %q, want plugin placeholder preserved with task run resolved", res.Group.Name)
	}
	if res.User.Login != "user-${PLUGIN_NAME}-task-1" {
		t.Fatalf("User.Login = %q, want plugin placeholder preserved with task run resolved", res.User.Login)
	}
	if res.Projects[0].Key != "${PLUGIN_NAME}-task-1" {
		t.Fatalf("Projects[0].Key = %q, want plugin placeholder preserved with task run resolved", res.Projects[0].Key)
	}
}

func TestRunCreate_ResolvedConfigExpandsAllTempResources(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	tokenPath := filepath.Join(dir, "token.env")
	statePath := filepath.Join(dir, "state.yaml")
	resolvedPath := filepath.Join(dir, "resolved.yaml")

	oldConfigFile := configFile
	oldTaskRunID := taskRunID
	oldPlugin := plugin
	oldResolvedConfig := resolvedConfig
	oldTokenFile := tokenFile
	oldStateFile := stateFile
	oldManagerTokenFile := managerTokenFile
	oldTempUserPasswordFile := tempUserPasswordFile
	t.Cleanup(func() {
		configFile = oldConfigFile
		taskRunID = oldTaskRunID
		plugin = oldPlugin
		resolvedConfig = oldResolvedConfig
		tokenFile = oldTokenFile
		stateFile = oldStateFile
		managerTokenFile = oldManagerTokenFile
		tempUserPasswordFile = oldTempUserPasswordFile
	})

	cfgData := `sonarqube:
  endpoint: https://sonarqube.example.com
  manager:
    username: manager
    token: manager-token
  temp_resources:
    - plugin_name: ${PLUGIN_NAME}
      group:
        name: templated-${TASK_RUN_ID}
        description: templated group
      user:
        login: user-${TASK_RUN_ID}
        name: Test User
        email: user-${TASK_RUN_ID}@example.com
        password: static-password
        groups:
          - ${PLUGIN_NAME}-group
      projects:
        - key: project-${TASK_RUN_ID}
          name: Project ${PLUGIN_NAME}
      permission_template:
        name: template-${TASK_RUN_ID}
        description: Template ${PLUGIN_NAME}
        project_key_pattern: project-${TASK_RUN_ID}.*
        permissions:
          - user
    - plugin_name: static
      group:
        name: ${PLUGIN_NAME}-${TASK_RUN_ID}
        description: static group
      user:
        login: ${PLUGIN_NAME}-user-${TASK_RUN_ID}
        name: Static User
        email: static@example.com
        password: static-password
        groups:
          - static-group
      projects:
        - key: static-project-${TASK_RUN_ID}
          name: Static Project
      permission_template:
        name: static-template-${TASK_RUN_ID}
        description: Static Template
        project_key_pattern: static-project-${TASK_RUN_ID}.*
        permissions:
          - user
`
	if err := os.WriteFile(cfgPath, []byte(cfgData), 0600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	configFile = cfgPath
	taskRunID = "task-1"
	plugin = "tektoncd"
	resolvedConfig = resolvedPath
	tokenFile = tokenPath
	stateFile = statePath
	managerTokenFile = ""
	tempUserPasswordFile = ""

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/authorizations/groups", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/api/users/create", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/user_groups/add_user", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/permissions/create_template", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/permissions/add_group_to_template", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/projects/create", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/user_tokens/generate", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"token":"generated-token"}`)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	t.Setenv("SONARQUBE_ALLOW_HTTP", "true")
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(cfgData, "https://sonarqube.example.com", server.URL, 1)), 0600); err != nil {
		t.Fatalf("WriteFile(updated config) error = %v", err)
	}

	if err := runCreate(createCmd, nil); err != nil {
		t.Fatalf("runCreate() error = %v", err)
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		t.Fatalf("ReadFile(resolved config) error = %v", err)
	}

	var resolved config.Config
	if err := yaml.Unmarshal(data, &resolved); err != nil {
		t.Fatalf("yaml.Unmarshal() error = %v", err)
	}

	if resolved.SonarQube.TempResources[0].Group.Name != "templated-task-1" {
		t.Fatalf("resolved first resource group = %q, want templated-task-1", resolved.SonarQube.TempResources[0].Group.Name)
	}
	if resolved.SonarQube.TempResources[1].Group.Name != "static-task-1" {
		t.Fatalf("resolved second resource group = %q, want static-task-1", resolved.SonarQube.TempResources[1].Group.Name)
	}
	if resolved.SonarQube.TempResources[1].User.Login != "static-user-task-1" {
		t.Fatalf("resolved second resource user login = %q, want static-user-task-1", resolved.SonarQube.TempResources[1].User.Login)
	}
	if resolved.SonarQube.TempResources[1].Group.Name == "tektoncd-task-1" {
		t.Fatalf("resolved second resource group = %q, want static plugin context instead of selected plugin", resolved.SonarQube.TempResources[1].Group.Name)
	}
}

func TestCreateResources_RollbackDeletesOnlyCreatedResourcesOnConflict(t *testing.T) {
	var deleteGroupCalls int
	var deleteUserCalls int
	var deleteProjectCalls int
	var deleteTemplateCalls int
	var revokeTokenCalls int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/authorizations/groups", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("/api/users/create", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errors":[{"msg":"User already exists"}]}`)
	})
	mux.HandleFunc("/api/v2/authorizations/groups/test-group", func(w http.ResponseWriter, r *http.Request) {
		deleteGroupCalls++
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/users/deactivate", func(w http.ResponseWriter, r *http.Request) {
		deleteUserCalls++
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/projects/delete", func(w http.ResponseWriter, r *http.Request) {
		deleteProjectCalls++
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/permissions/delete_template", func(w http.ResponseWriter, r *http.Request) {
		deleteTemplateCalls++
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/user_tokens/revoke", func(w http.ResponseWriter, r *http.Request) {
		revokeTokenCalls++
		w.WriteHeader(http.StatusOK)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	c := sqclient.New(server.URL, "test-token")
	res := config.TempResource{
		PluginName: "tektoncd",
		Group: config.Group{
			Name:        "test-group",
			Description: "Temporary group",
		},
		User: config.User{
			Login:    "test-user",
			Name:     "Test User",
			Email:    "test@example.com",
			Password: "password",
		},
		Projects: []config.Project{
			{Key: "test-project", Name: "Test Project"},
		},
		PermissionTemplate: config.PermissionTemplate{
			Name: "test-template",
		},
	}

	token, plan, err := createResources(c, res, "task-1")
	if token != "" {
		t.Fatalf("createResources() token = %q, want empty", token)
	}
	if plan == nil {
		t.Fatal("createResources() plan = nil")
	}
	if !errors.Is(err, sqclient.ErrAlreadyExists) {
		t.Fatalf("createResources() error = %v, want ErrAlreadyExists", err)
	}
	if deleteGroupCalls != 1 {
		t.Fatalf("deleteGroupCalls = %d, want 1", deleteGroupCalls)
	}
	if deleteUserCalls != 0 || deleteProjectCalls != 0 || deleteTemplateCalls != 0 || revokeTokenCalls != 0 {
		t.Fatalf("unexpected cleanup calls: user=%d project=%d template=%d token=%d", deleteUserCalls, deleteProjectCalls, deleteTemplateCalls, revokeTokenCalls)
	}
}

func TestCleanupOnError_ReportsCleanupFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/authorizations/groups/test-group", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"errors":[{"msg":"delete failed"}]}`)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	c := sqclient.New(server.URL, "test-token")
	res := config.TempResource{
		PluginName: "tektoncd",
		Group: config.Group{
			Name: "test-group",
		},
	}
	plan := &cleanupPlan{deleteGroup: true}

	err := cleanupOnError(c, res, plan, "task-1", errors.New("write failed"))
	if err == nil {
		t.Fatal("cleanupOnError() error = nil, want cleanup failure")
	}
	if !strings.Contains(err.Error(), "write failed") || !strings.Contains(err.Error(), "cleanup failed") {
		t.Fatalf("cleanupOnError() error = %v, want original and cleanup failure details", err)
	}
}

func TestSelectTempResource_RejectsMultipleMatches(t *testing.T) {
	cfg := &config.Config{
		SonarQube: config.SonarQubeConfig{
			TempResources: []config.TempResource{
				{PluginName: "tektoncd", Group: config.Group{Name: "group-1"}, User: config.User{Login: "user-1"}},
				{PluginName: "${PLUGIN_NAME}", Group: config.Group{Name: "group-2"}, User: config.User{Login: "user-2"}},
			},
		},
	}

	_, _, err := selectTempResource(cfg, "tektoncd", "task-1")
	if err == nil || !strings.Contains(err.Error(), "multiple temp_resources matched plugin") {
		t.Fatalf("selectTempResource() error = %v, want duplicate match error", err)
	}
}

func TestResourceState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "resource-state.yaml")
	want := resourceState{
		Version:            1,
		Plugin:             "tektoncd",
		Endpoint:           "https://sonarqube.example.com",
		TaskRunID:          "task-1",
		GroupName:          "test-group",
		UserLogin:          "test-user",
		ProjectKeys:        []string{"project-a"},
		PermissionTemplate: "test-template",
		TokenName:          "test-token-task-1",
	}

	if err := writeResourceState(path, want); err != nil {
		t.Fatalf("writeResourceState() error = %v", err)
	}

	got, err := loadResourceState(path)
	if err != nil {
		t.Fatalf("loadResourceState() error = %v", err)
	}
	if got.Plugin != want.Plugin || got.Endpoint != want.Endpoint || got.TaskRunID != want.TaskRunID || got.GroupName != want.GroupName || got.TokenName != want.TokenName {
		t.Fatalf("state round trip mismatch: got %+v want %+v", got, want)
	}
}

func TestRunCleanup_UsesStateFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	statePath := filepath.Join(dir, "state.yaml")
	oldConfigFile := configFile
	oldPlugin := plugin
	oldStateFile := stateFile
	t.Cleanup(func() {
		configFile = oldConfigFile
		plugin = oldPlugin
		stateFile = oldStateFile
	})

	cfgData := []byte(`sonarqube:
  endpoint: https://sonarqube.example.com
  manager:
    username: manager
    token: token
  temp_resources:
    - plugin_name: unrelated
      group:
        name: ""
      user:
        login: ""
`)
	if err := os.WriteFile(cfgPath, cfgData, 0600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	configFile = cfgPath
	stateFile = statePath
	plugin = "tektoncd"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/user_tokens/revoke", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/api/projects/delete", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if got := r.PostForm.Get("project"); got != "state-project" {
			t.Fatalf("project delete target = %q, want state-project", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/permissions/delete_template", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if got := r.PostForm.Get("templateName"); got != "state-template" {
			t.Fatalf("template delete target = %q, want state-template", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/users/deactivate", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm() error = %v", err)
		}
		if got := r.PostForm.Get("login"); got != "state-user" {
			t.Fatalf("user delete target = %q, want state-user", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/v2/authorizations/groups/state-group", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	server := httptest.NewTLSServer(mux)
	defer server.Close()

	if err := writeResourceState(statePath, resourceState{
		Version:            1,
		Plugin:             "tektoncd",
		Endpoint:           strings.TrimSuffix(server.URL, "/"),
		TaskRunID:          "task-1",
		GroupName:          "state-group",
		UserLogin:          "state-user",
		ProjectKeys:        []string{"state-project"},
		PermissionTemplate: "state-template",
		TokenName:          "test-token-task-1",
	}); err != nil {
		t.Fatalf("writeResourceState(state) error = %v", err)
	}

	t.Setenv("SONARQUBE_ALLOW_HTTP", "true")
	cfgData = []byte(fmt.Sprintf(`sonarqube:
  endpoint: %s
  manager:
    username: manager
    token: token
  temp_resources:
    - plugin_name: unrelated
      group:
        name: ""
      user:
        login: ""
`, server.URL))
	if err := os.WriteFile(cfgPath, cfgData, 0600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}

	origTransport := http.DefaultTransport
	server.Client()
	t.Cleanup(func() {
		http.DefaultTransport = origTransport
	})

	client := server.Client()
	http.DefaultTransport = client.Transport

	if err := runCleanup(cleanupCmd, nil); err != nil {
		t.Fatalf("runCleanup() error = %v", err)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file still exists after cleanup: %v", err)
	}
}

func TestRunCleanup_RejectsEndpointMismatch(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	statePath := filepath.Join(dir, "state.yaml")
	oldConfigFile := configFile
	oldPlugin := plugin
	oldStateFile := stateFile
	t.Cleanup(func() {
		configFile = oldConfigFile
		plugin = oldPlugin
		stateFile = oldStateFile
	})

	cfgData := []byte(`sonarqube:
  endpoint: https://sonarqube-a.example.com
  manager:
    username: manager
    token: token
`)
	if err := os.WriteFile(cfgPath, cfgData, 0600); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	if err := writeResourceState(statePath, resourceState{
		Version:            1,
		Plugin:             "tektoncd",
		Endpoint:           "https://sonarqube-b.example.com",
		TaskRunID:          "task-1",
		GroupName:          "state-group",
		UserLogin:          "state-user",
		ProjectKeys:        []string{"state-project"},
		PermissionTemplate: "state-template",
		TokenName:          "test-token-task-1",
	}); err != nil {
		t.Fatalf("writeResourceState(state) error = %v", err)
	}

	configFile = cfgPath
	stateFile = statePath
	plugin = "tektoncd"

	err := runCleanup(cleanupCmd, nil)
	if err == nil || !strings.Contains(err.Error(), "endpoint mismatch") {
		t.Fatalf("runCleanup() error = %v, want endpoint mismatch", err)
	}
}

func TestCleanupOnError_RemovesStateFileAfterSuccessfulRollback(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "resource-state.yaml")
	tokenPath := filepath.Join(dir, "token.env")
	if err := os.WriteFile(statePath, []byte("state"), 0600); err != nil {
		t.Fatalf("WriteFile(state) error = %v", err)
	}
	if err := os.WriteFile(tokenPath, []byte("token"), 0600); err != nil {
		t.Fatalf("WriteFile(token) error = %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/authorizations/groups/test-group", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	c := sqclient.New(server.URL, "test-token")
	res := config.TempResource{
		PluginName: "tektoncd",
		Group:      config.Group{Name: "test-group"},
	}

	err := cleanupOnError(c, res, &cleanupPlan{deleteGroup: true}, "task-1", errors.New("post-create failed"), statePath, tokenPath)
	if err == nil || !strings.Contains(err.Error(), "post-create failed") {
		t.Fatalf("cleanupOnError() error = %v, want original error", err)
	}
	if _, statErr := os.Stat(statePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("state file still exists after successful rollback: %v", statErr)
	}
	if _, statErr := os.Stat(tokenPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("token file still exists after successful rollback: %v", statErr)
	}
}
