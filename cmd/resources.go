package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tektoncd/operator/tools/sonarqube-cli/pkg/client"
	"github.com/tektoncd/operator/tools/sonarqube-cli/pkg/config"
	"gopkg.in/yaml.v3"
)

var resourcesCmd = &cobra.Command{
	Use:   "resources",
	Short: "Manage SonarQube temporary resources",
}

var createCmd = &cobra.Command{
	Use:   "create",
	Short: "Create temporary resources",
	RunE:  runCreate,
}

var cleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Cleanup temporary resources",
	RunE:  runCleanup,
}

var (
	configFile           string
	taskRunID            string
	plugin               string
	resolvedConfig       string
	outputTemplate       string
	outputFile           string
	tokenFile            string
	stateFile            string
	managerTokenFile     string
	tempUserPasswordFile string
)

func init() {
	resourcesCmd.AddCommand(createCmd, cleanupCmd)

	createCmd.Flags().StringVar(&configFile, "config", "", "Config file path")
	createCmd.Flags().StringVar(&taskRunID, "task-run-id", "", "Tekton TaskRun ID (only needed if config is template)")
	createCmd.Flags().StringVar(&plugin, "plugin", "", "Plugin name")
	createCmd.Flags().StringVar(&resolvedConfig, "resolved-config", "", "Path to save resolved config")
	createCmd.Flags().StringVar(&outputTemplate, "output-template", "", "Path to output template")
	createCmd.Flags().StringVar(&outputFile, "output-file", "", "Path to output file")
	createCmd.Flags().StringVar(&tokenFile, "token-file", "", "Write token to file (recommended for security)")
	createCmd.Flags().StringVar(&stateFile, "state-file", "", "Path to save cleanup state")
	createCmd.Flags().StringVar(&managerTokenFile, "manager-token-file", "", "Path to file containing manager token (preferred over SONARQUBE_MANAGER_TOKEN)")
	createCmd.Flags().StringVar(&tempUserPasswordFile, "temp-user-password-file", "", "Path to file containing temporary user password (preferred over TEMP_USER_PASSWORD)")
	createCmd.MarkFlagRequired("config")
	createCmd.MarkFlagRequired("plugin")
	createCmd.MarkFlagRequired("token-file")
	createCmd.MarkFlagRequired("state-file")

	cleanupCmd.Flags().StringVar(&configFile, "config", "", "Config file path")
	cleanupCmd.Flags().StringVar(&plugin, "plugin", "", "Plugin name (optional sanity check)")
	cleanupCmd.Flags().StringVar(&stateFile, "state-file", "", "Path to cleanup state")
	cleanupCmd.Flags().StringVar(&managerTokenFile, "manager-token-file", "", "Path to file containing manager token (preferred over SONARQUBE_MANAGER_TOKEN)")
	cleanupCmd.MarkFlagRequired("config")
	cleanupCmd.MarkFlagRequired("state-file")
}

type cleanupPlan struct {
	revokeToken              bool
	tokenName                string
	deleteProjects           []string
	deletePermissionTemplate bool
	deleteUser               bool
	deleteGroup              bool
}

// empty reports whether the plan contains any actionable cleanup step.
func (p *cleanupPlan) empty() bool {
	if p == nil {
		return true
	}
	// Treat an all-false plan as a no-op so callers can safely invoke cleanup helpers after partial failures.
	return !p.revokeToken &&
		len(p.deleteProjects) == 0 &&
		!p.deletePermissionTemplate &&
		!p.deleteUser &&
		!p.deleteGroup
}

// resourceState is the persisted create-result snapshot used by cleanup.
type resourceState struct {
	Version            int      `yaml:"version"`
	Plugin             string   `yaml:"plugin"`
	Endpoint           string   `yaml:"endpoint"`
	TaskRunID          string   `yaml:"task_run_id,omitempty"`
	GroupName          string   `yaml:"group_name"`
	UserLogin          string   `yaml:"user_login"`
	ProjectKeys        []string `yaml:"project_keys,omitempty"`
	PermissionTemplate string   `yaml:"permission_template"`
	TokenName          string   `yaml:"token_name"`
}

type outputTarget struct {
	flag string
	path string
}

// writeFileAtomically writes data to a temp file and renames it into place.
func writeFileAtomically(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	removeTmp := true
	defer func() {
		_ = tmpFile.Close()
		if removeTmp {
			// Best-effort cleanup prevents leaked temp files when a later step fails.
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmpFile.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmpFile.Write(data); err != nil {
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	removeTmp = false
	return nil
}

// selectTempResource resolves and picks exactly one temp resource for a plugin.
func selectTempResource(cfg *config.Config, pluginName, taskRunID string) (config.TempResource, int, error) {
	var (
		matched    config.TempResource
		matchedIdx = -1
	)

	for i, res := range cfg.SonarQube.TempResources {
		resolvedPluginName, err := config.ReplaceVariables(res.PluginName, taskRunID, pluginName)
		if err != nil {
			return config.TempResource{}, -1, err
		}
		if resolvedPluginName != pluginName {
			continue
		}

		res.PluginName = resolvedPluginName
		if err := config.ApplyTemplate(&res, taskRunID, pluginName); err != nil {
			return config.TempResource{}, -1, err
		}
		if matchedIdx >= 0 {
			// Matching more than one entry is ambiguous after template expansion, so fail instead of picking arbitrarily.
			return config.TempResource{}, -1, fmt.Errorf("multiple temp_resources matched plugin %s; expected exactly one", pluginName)
		}

		matched = res
		matchedIdx = i
	}

	if matchedIdx < 0 {
		return config.TempResource{}, -1, fmt.Errorf("plugin not found: %s", pluginName)
	}

	return matched, matchedIdx, nil
}

// writeResourceState persists cleanup metadata after create succeeds.
func writeResourceState(path string, state resourceState) error {
	data, err := yaml.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}
	if err := writeFileAtomically(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}
	return nil
}

// loadResourceState reads and validates the persisted cleanup metadata.
func loadResourceState(path string) (*resourceState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read state file: %w", err)
	}

	var state resourceState
	if err := yaml.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to parse state file: %w", err)
	}
	// Validate the persisted identifiers up front so cleanup never guesses which remote objects to delete.
	if state.Version != 1 {
		return nil, fmt.Errorf("unsupported state file version: %d", state.Version)
	}
	if state.Plugin == "" {
		return nil, fmt.Errorf("state file missing plugin")
	}
	if state.Endpoint == "" {
		return nil, fmt.Errorf("state file missing endpoint")
	}
	if state.GroupName == "" {
		return nil, fmt.Errorf("state file missing group_name")
	}
	if state.UserLogin == "" {
		return nil, fmt.Errorf("state file missing user_login")
	}
	if state.PermissionTemplate == "" {
		return nil, fmt.Errorf("state file missing permission_template")
	}
	if state.TokenName == "" {
		return nil, fmt.Errorf("state file missing token_name")
	}

	return &state, nil
}

func normalizeEndpoint(endpoint string) string {
	// Normalize trailing slashes so create and cleanup compare the same endpoint identity.
	return strings.TrimSuffix(endpoint, "/")
}

// parseOutputEndpoint splits a SonarQube endpoint into the template fields consumed by e2e configs.
func parseOutputEndpoint(endpoint string) (string, int, string, error) {
	parsedURL, err := url.Parse(endpoint)
	if err != nil {
		return "", 0, "", fmt.Errorf("invalid sonarqube.endpoint for output template: %w", err)
	}
	if parsedURL.Hostname() == "" {
		return "", 0, "", fmt.Errorf("sonarqube.endpoint host is required for output template")
	}

	scheme := strings.ToLower(parsedURL.Scheme)
	port := 0

	// Keep template output stable even when callers omit the default port in the endpoint URL.
	if parsedURL.Port() != "" {
		parsedPort, err := strconv.Atoi(parsedURL.Port())
		if err != nil {
			return "", 0, "", fmt.Errorf("invalid sonarqube.endpoint port for output template: %w", err)
		}
		port = parsedPort
	} else {
		switch scheme {
		case "https":
			port = 443
		case "http":
			port = 80
		default:
			return "", 0, "", fmt.Errorf("unsupported sonarqube.endpoint scheme for output template: %s", parsedURL.Scheme)
		}
	}

	return parsedURL.Hostname(), port, scheme, nil
}

// buildResourceState captures created resource identifiers for deterministic cleanup.
func buildResourceState(pluginName, endpoint string, res config.TempResource, taskRunID string) resourceState {
	tokenName := fmt.Sprintf("test-token-%s", taskRunID)
	if taskRunID == "" {
		tokenName = "test-token"
	}

	projectKeys := make([]string, 0, len(res.Projects))
	for _, proj := range res.Projects {
		projectKeys = append(projectKeys, proj.Key)
	}

	return resourceState{
		Version:            1,
		Plugin:             pluginName,
		Endpoint:           normalizeEndpoint(endpoint),
		TaskRunID:          taskRunID,
		GroupName:          res.Group.Name,
		UserLogin:          res.User.Login,
		ProjectKeys:        projectKeys,
		PermissionTemplate: res.PermissionTemplate.Name,
		TokenName:          tokenName,
	}
}

// stateToCleanupPlan reconstructs the managed cleanup actions from saved state.
func stateToCleanupPlan(state *resourceState) *cleanupPlan {
	if state == nil {
		return nil
	}
	// The state file is written only after create succeeds, so cleanup should attempt the full managed resource set.
	return &cleanupPlan{
		revokeToken:              true,
		tokenName:                state.TokenName,
		deleteProjects:           append([]string(nil), state.ProjectKeys...),
		deletePermissionTemplate: true,
		deleteUser:               true,
		deleteGroup:              true,
	}
}

// stateToResource rebuilds the minimum resource identity payload required by cleanup.
func stateToResource(state *resourceState) config.TempResource {
	if state == nil {
		return config.TempResource{}
	}
	return config.TempResource{
		PluginName: state.Plugin,
		Group: config.Group{
			Name: state.GroupName,
		},
		User: config.User{
			Login: state.UserLogin,
		},
		Projects: func() []config.Project {
			// Rebuild the minimal project identities from state so cleanup does not depend on config drift.
			projects := make([]config.Project, 0, len(state.ProjectKeys))
			for _, key := range state.ProjectKeys {
				projects = append(projects, config.Project{Key: key})
			}
			return projects
		}(),
		PermissionTemplate: config.PermissionTemplate{
			Name: state.PermissionTemplate,
		},
	}
}

func removeLocalFiles(paths ...string) error {
	for _, path := range paths {
		if path == "" {
			// Optional outputs are passed through the same helper, so ignore unset paths.
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("failed to remove local file %s: %w", path, err)
		}
	}
	return nil
}

func validateCreateOutputTargets(targets ...outputTarget) error {
	seen := make(map[string]string, len(targets))
	for _, target := range targets {
		if target.path == "" {
			continue
		}

		absPath, err := filepath.Abs(target.path)
		if err != nil {
			return fmt.Errorf("failed to resolve %s path %s: %w", target.flag, target.path, err)
		}
		// Compare absolute paths so different relative spellings of the same target still conflict.
		if otherFlag, ok := seen[absPath]; ok {
			return fmt.Errorf("%s path %s conflicts with %s; output targets must be distinct", target.flag, target.path, otherFlag)
		}
		seen[absPath] = target.flag

		info, err := os.Stat(target.path)
		if err == nil {
			targetType := "file"
			if info.IsDir() {
				targetType = "directory"
			}
			return fmt.Errorf("%s already exists at %s; refusing to overwrite existing %s", target.flag, target.path, targetType)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("failed to stat %s path %s: %w", target.flag, target.path, err)
		}
	}
	return nil
}

func replaceVariablesForOutput(s, taskRunID, pluginName string) string {
	// Output rendering is best-effort: unresolved placeholders are preserved instead of failing the whole command.
	if taskRunID != "" {
		s = strings.ReplaceAll(s, "{{TASK_RUN_ID}}", taskRunID)
		s = strings.ReplaceAll(s, "${TASK_RUN_ID}", taskRunID)
	}
	if pluginName != "" {
		s = strings.ReplaceAll(s, "{{PLUGIN_NAME}}", pluginName)
		s = strings.ReplaceAll(s, "${PLUGIN_NAME}", pluginName)
	}
	return s
}

func containsPluginTemplate(s string) bool {
	return strings.Contains(s, "{{PLUGIN_NAME}}") || strings.Contains(s, "${PLUGIN_NAME}")
}

func resolveTempResourceForOutput(res config.TempResource, taskRunID, pluginName string, resolvePluginTemplate bool) config.TempResource {
	resolvedPluginName := res.PluginName
	if resolvePluginTemplate {
		resolvedPluginName = replaceVariablesForOutput(res.PluginName, taskRunID, pluginName)
	} else {
		// Keep unmatched plugin placeholders intact so the exported config remains reusable for other plugins.
		resolvedPluginName = replaceVariablesForOutput(res.PluginName, taskRunID, "")
	}

	outputPluginName := resolvedPluginName
	if containsPluginTemplate(outputPluginName) {
		// Avoid partially expanding dependent fields with a literal placeholder when plugin_name is still unresolved.
		outputPluginName = ""
	}

	res.PluginName = resolvedPluginName
	res.Group.Name = replaceVariablesForOutput(res.Group.Name, taskRunID, outputPluginName)
	res.Group.Description = replaceVariablesForOutput(res.Group.Description, taskRunID, outputPluginName)
	res.User.Login = replaceVariablesForOutput(res.User.Login, taskRunID, outputPluginName)
	res.User.Name = replaceVariablesForOutput(res.User.Name, taskRunID, outputPluginName)
	res.User.Email = replaceVariablesForOutput(res.User.Email, taskRunID, outputPluginName)
	res.User.Password = replaceVariablesForOutput(res.User.Password, taskRunID, outputPluginName)

	for i := range res.User.Groups {
		res.User.Groups[i] = replaceVariablesForOutput(res.User.Groups[i], taskRunID, outputPluginName)
	}
	for i := range res.Projects {
		res.Projects[i].Key = replaceVariablesForOutput(res.Projects[i].Key, taskRunID, outputPluginName)
		res.Projects[i].Name = replaceVariablesForOutput(res.Projects[i].Name, taskRunID, outputPluginName)
	}

	res.PermissionTemplate.Name = replaceVariablesForOutput(res.PermissionTemplate.Name, taskRunID, outputPluginName)
	res.PermissionTemplate.Description = replaceVariablesForOutput(res.PermissionTemplate.Description, taskRunID, outputPluginName)
	res.PermissionTemplate.ProjectKeyPattern = replaceVariablesForOutput(res.PermissionTemplate.ProjectKeyPattern, taskRunID, outputPluginName)
	return res
}

func resolveTaskRunID(flagValue string) string {
	if flagValue != "" {
		// The explicit flag wins so retries can override inherited CI environment values.
		return flagValue
	}
	return os.Getenv("TASK_RUN_ID")
}

// runCreate creates SonarQube resources and writes local artifacts for downstream tasks.
func runCreate(cmd *cobra.Command, args []string) error {
	if tokenFile == "" {
		return fmt.Errorf("--token-file is required for security (prevents token leakage to logs)")
	}
	if stateFile == "" {
		return fmt.Errorf("--state-file is required")
	}
	if err := validateCreateOutputTargets(
		outputTarget{flag: "--state-file", path: stateFile},
		outputTarget{flag: "--token-file", path: tokenFile},
		outputTarget{flag: "--resolved-config", path: resolvedConfig},
		outputTarget{flag: "--output-file", path: func() string {
			if outputTemplate == "" {
				// Ignore --output-file when template rendering is disabled so it does not block unrelated create flows.
				return ""
			}
			return outputFile
		}()},
	); err != nil {
		return err
	}

	overrides, err := resolveConfigOverrides(managerTokenFile, tempUserPasswordFile)
	if err != nil {
		return err
	}

	cfg, err := config.LoadWithVariables(configFile, overrides)
	if err != nil {
		return err
	}

	effectiveTaskRunID := resolveTaskRunID(taskRunID)
	c := client.New(cfg.SonarQube.Endpoint, cfg.SonarQube.Manager.Token)
	res, matchedIdx, err := selectTempResource(cfg, plugin, effectiveTaskRunID)
	if err != nil {
		return err
	}

	// Persist the resolved resource back into cfg so --resolved-config exports the exact object that was created.
	cfg.SonarQube.TempResources[matchedIdx] = res
	localFiles := make([]string, 0, 4)

	token, plan, err := createResources(c, res, effectiveTaskRunID)
	if err != nil {
		return err
	}

	state := buildResourceState(plugin, cfg.SonarQube.Endpoint, res, effectiveTaskRunID)
	if err := writeResourceState(stateFile, state); err != nil {
		return cleanupOnError(c, res, plan, effectiveTaskRunID, fmt.Errorf("failed to write state file: %w", err), stateFile)
	}
	localFiles = append(localFiles, stateFile)

	// Write the cleanup state before any optional outputs so later rollback still knows what was created remotely.
	tokenData := fmt.Sprintf("SONARQUBE_TOKEN=%s\nSONARQUBE_USER=%s\n", token, res.User.Login)
	if err := writeFileAtomically(tokenFile, []byte(tokenData), 0600); err != nil {
		return cleanupOnError(c, res, plan, effectiveTaskRunID, fmt.Errorf("failed to write token file: %w", err), localFiles...)
	}
	localFiles = append(localFiles, tokenFile)
	fmt.Printf("Token written to %s\n", tokenFile)

	if resolvedConfig != "" {
		fmt.Fprintf(os.Stderr, "- Saving resolved config to %s...\n", resolvedConfig)
		for i := range cfg.SonarQube.TempResources {
			cfg.SonarQube.TempResources[i] = resolveTempResourceForOutput(cfg.SonarQube.TempResources[i], effectiveTaskRunID, plugin, i == matchedIdx)
		}
		data, err := yaml.Marshal(cfg)
		if err != nil {
			return cleanupOnError(c, res, plan, effectiveTaskRunID, fmt.Errorf("failed to marshal config: %w", err), localFiles...)
		}
		if err := writeFileAtomically(resolvedConfig, data, 0600); err != nil {
			return cleanupOnError(c, res, plan, effectiveTaskRunID, fmt.Errorf("failed to write resolved config: %w", err), localFiles...)
		}
		localFiles = append(localFiles, resolvedConfig)
	}

	if outputTemplate != "" && outputFile != "" {
		fmt.Fprintf(os.Stderr, "- Rendering output template to %s...\n", outputFile)
		host, port, scheme, err := parseOutputEndpoint(cfg.SonarQube.Endpoint)
		if err != nil {
			return cleanupOnError(c, res, plan, effectiveTaskRunID, err, localFiles...)
		}
		data := map[string]interface{}{
			"Endpoint": cfg.SonarQube.Endpoint,
			"Host":     host,
			"Port":     port,
			"Scheme":   scheme,
			"Users": []map[string]string{
				{
					"Login":    res.User.Login,
					"Password": res.User.Password,
					"Token":    token,
				},
			},
			"Projects": res.Projects,
		}
		if err := config.RenderTemplate(outputTemplate, outputFile, data); err != nil {
			return cleanupOnError(c, res, plan, effectiveTaskRunID, err, localFiles...)
		}
		localFiles = append(localFiles, outputFile)
	}

	fmt.Printf("Created resources for plugin: %s\n", plugin)
	return nil
}

// createResources creates remote resources in dependency order and records rollback steps.
func createResources(c *client.Client, res config.TempResource, taskRunID string) (string, *cleanupPlan, error) {
	fmt.Fprintf(os.Stderr, "Creating resources for plugin: %s (taskRunID: %s)...\n", res.PluginName, taskRunID)
	plan := &cleanupPlan{}
	tokenName := fmt.Sprintf("test-token-%s", taskRunID)
	if taskRunID == "" {
		tokenName = "test-token"
	}
	plan.tokenName = tokenName

	fmt.Fprintf(os.Stderr, "- Creating group %s...\n", res.Group.Name)
	groupCreated, err := c.CreateGroup(res.Group.Name, res.Group.Description)
	if groupCreated {
		// Record only resources created in this run so cleanup does not delete pre-existing shared objects.
		plan.deleteGroup = true
	}
	if err != nil {
		return "", plan, cleanupOnError(c, res, plan, taskRunID, err)
	}

	fmt.Fprintf(os.Stderr, "- Creating user %s...\n", res.User.Login)
	userCreated, err := c.CreateUser(res.User)
	if userCreated {
		// User creation can succeed before later steps fail, so track it independently for rollback.
		plan.deleteUser = true
	}
	if err != nil {
		return "", plan, cleanupOnError(c, res, plan, taskRunID, err)
	}

	fmt.Fprintf(os.Stderr, "- Setting global permissions...\n")
	for _, perm := range res.GlobalPermissions {
		if err := c.AddGlobalPermission(res.User.Login, perm); err != nil {
			return "", plan, cleanupOnError(c, res, plan, taskRunID, err)
		}
	}

	fmt.Fprintf(os.Stderr, "- Creating permission template %s...\n", res.PermissionTemplate.Name)
	templateCreated, err := c.CreatePermissionTemplate(res.PermissionTemplate, res.Group.Name)
	if templateCreated {
		// Keep rollback granular because template assignment happens after template creation.
		plan.deletePermissionTemplate = true
	}
	if err != nil {
		return "", plan, cleanupOnError(c, res, plan, taskRunID, err)
	}

	for _, proj := range res.Projects {
		fmt.Fprintf(os.Stderr, "- Creating project %s...\n", proj.Key)
		projectCreated, err := c.CreateProject(proj)
		if projectCreated {
			// Append per project so partial success still cleans up the subset created before a later failure.
			plan.deleteProjects = append(plan.deleteProjects, proj.Key)
		}
		if err != nil {
			return "", plan, cleanupOnError(c, res, plan, taskRunID, err)
		}
	}

	fmt.Fprintf(os.Stderr, "- Generating user token %s...\n", tokenName)
	token, err := c.GenerateUserToken(res.User.Login, tokenName)
	if err != nil {
		return "", plan, cleanupOnError(c, res, plan, taskRunID, err)
	}
	plan.revokeToken = true

	return token, plan, nil
}

// cleanupOnError executes rollback and local file cleanup while preserving the original failure.
func cleanupOnError(c *client.Client, res config.TempResource, plan *cleanupPlan, taskRunID string, originalErr error, cleanupPaths ...string) error {
	if plan == nil || plan.empty() {
		return originalErr
	}
	// Try remote rollback before deleting local artifacts so operators can still inspect state when cleanup fails.
	if clErr := cleanupResources(c, res, plan, taskRunID); clErr != nil {
		return fmt.Errorf("%w (cleanup failed: %v)", originalErr, clErr)
	}
	if err := removeLocalFiles(cleanupPaths...); err != nil {
		return fmt.Errorf("%w (%v)", originalErr, err)
	}
	return originalErr
}

// cleanupResources deletes previously created resources according to the supplied cleanup plan.
func cleanupResources(c *client.Client, res config.TempResource, plan *cleanupPlan, taskRunID string) error {
	if plan == nil || plan.empty() {
		return nil
	}

	var errs []error
	tokenName := plan.tokenName
	if tokenName == "" {
		// Recompute the legacy token name when the caller did not persist it in the cleanup plan or state file.
		tokenName = fmt.Sprintf("test-token-%s", taskRunID)
		if taskRunID == "" {
			tokenName = "test-token"
		}
	}
	// Revoke token if it exists
	fmt.Fprintf(os.Stderr, "Cleaning up resources for plugin: %s (taskRunID: %s)...\n", res.PluginName, taskRunID)

	if plan.revokeToken {
		fmt.Fprintf(os.Stderr, "- Revoking token %s for user %s...\n", tokenName, res.User.Login)
		if err := c.RevokeUserToken(res.User.Login, tokenName); err != nil {
			errs = append(errs, fmt.Errorf("revoke token: %w", err))
		}
	}

	for _, projKey := range plan.deleteProjects {
		fmt.Fprintf(os.Stderr, "- Deleting project %s...\n", projKey)
		if err := c.DeleteProject(projKey); err != nil {
			errs = append(errs, fmt.Errorf("delete project %s: %w", projKey, err))
		}
	}

	// Delete in reverse dependency order so template, user, and group removal sees fewer attached resources.
	if plan.deletePermissionTemplate {
		fmt.Fprintf(os.Stderr, "- Deleting template %s...\n", res.PermissionTemplate.Name)
		if err := c.DeletePermissionTemplate(res.PermissionTemplate.Name); err != nil {
			errs = append(errs, fmt.Errorf("delete template: %w", err))
		}
	}
	if plan.deleteUser {
		fmt.Fprintf(os.Stderr, "- Deleting user %s...\n", res.User.Login)
		if err := c.DeleteUser(res.User.Login); err != nil {
			errs = append(errs, fmt.Errorf("delete user: %w", err))
		}
	}
	if plan.deleteGroup {
		fmt.Fprintf(os.Stderr, "- Deleting group %s...\n", res.Group.Name)
		if err := c.DeleteGroup(res.Group.Name); err != nil {
			errs = append(errs, fmt.Errorf("delete group: %w", err))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// runCleanup loads persisted state and removes managed resources from SonarQube.
func runCleanup(cmd *cobra.Command, args []string) error {
	overrides, err := resolveConfigOverrides(managerTokenFile, "")
	if err != nil {
		return err
	}

	endpoint, token, err := config.LoadConnectionWithVariables(configFile, overrides)
	if err != nil {
		return err
	}
	state, err := loadResourceState(stateFile)
	if err != nil {
		return err
	}
	if plugin != "" && plugin != state.Plugin {
		// Treat an explicit plugin mismatch as operator error before touching remote resources.
		return fmt.Errorf("plugin mismatch: state file is for %s, got %s", state.Plugin, plugin)
	}
	if normalizeEndpoint(endpoint) != state.Endpoint {
		// Refuse cleanup against a different endpoint even if credentials are otherwise valid.
		return fmt.Errorf("endpoint mismatch: state file is for %s, got %s", state.Endpoint, normalizeEndpoint(endpoint))
	}

	c := client.New(endpoint, token)
	// Reconstruct cleanup inputs from state so cleanup still works when temp_resources changed after creation.
	res := stateToResource(state)
	plan := stateToCleanupPlan(state)
	if err := cleanupResources(c, res, plan, state.TaskRunID); err != nil {
		return fmt.Errorf("cleanup failed for plugin %s: %w", state.Plugin, err)
	}
	if err := removeLocalFiles(stateFile); err != nil {
		return fmt.Errorf("cleanup succeeded for plugin %s but %v", state.Plugin, err)
	}

	fmt.Printf("Cleaned up resources for plugin: %s\n", state.Plugin)
	return nil
}
