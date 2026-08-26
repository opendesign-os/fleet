package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/sirupsen/logrus"
)

const defaultScriptsPerPage = 50

func registerScriptTools(s *server.MCPServer, fleetClient *FleetClient) {
	registerGetScripts(s, fleetClient)
	registerGetScript(s, fleetClient)
	registerCreateScript(s, fleetClient)
	registerUpdateScript(s, fleetClient)
	registerDeleteScript(s, fleetClient)
	registerRunScript(s, fleetClient)
	registerGetScriptResult(s, fleetClient)
	registerRunScriptBatch(s, fleetClient)
	registerGetScriptBatchStatus(s, fleetClient)
}

func registerGetScripts(s *server.MCPServer, fleetClient *FleetClient) {
	tool := mcp.NewTool("get_scripts",
		mcp.WithDescription("List saved scripts, scoped to one fleet. Scripts belong to exactly one fleet — omit `fleet` to list the Unassigned scope, pass a fleet name to list that fleet's scripts. Returns metadata only (id, name, fleet, timestamps); use get_script to read a script's body. Discover fleet names with get_fleets."),
		mcp.WithString("fleet", mcp.Description("Fleet name (e.g. 'Workstations'). Omit for the Unassigned scope.")),
		mcp.WithString("per_page", mcp.Description("Max number of scripts to return (default 50, max 200).")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
	)
	s.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		logrus.Info("Tool invoked: get_scripts")

		teamID, err := fleetClient.resolveFleetID(ctx, getOptionalString(request, "fleet"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		scripts, err := fleetClient.GetScripts(ctx, teamID, parsePerPageArg(request, defaultScriptsPerPage))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to get scripts: %v", err)), nil
		}
		return jsonResult(scripts)
	})
}

func registerGetScript(s *server.MCPServer, fleetClient *FleetClient) {
	tool := mcp.NewTool("get_script",
		mcp.WithDescription("Get one saved script by ID, including its body when `contents` is 'true'. Use get_scripts to discover IDs."),
		mcp.WithString("script_id", mcp.Required(), mcp.Description("Numeric script ID (from get_scripts).")),
		mcp.WithString("contents", mcp.Description("'true' to include the script body. Defaults to metadata only.")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
	)
	s.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		logrus.Info("Tool invoked: get_script")

		scriptID, err := parsePositiveUintString("script_id", getOptionalString(request, "script_id"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		withContents := strings.EqualFold(strings.TrimSpace(getOptionalString(request, "contents")), "true")
		script, err := fleetClient.GetScript(ctx, uint(scriptID), withContents)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to get script: %v", err)), nil
		}
		return jsonResult(script)
	})
}

func registerCreateScript(s *server.MCPServer, fleetClient *FleetClient) {
	tool := mcp.NewTool("create_script",
		mcp.WithDescription("Save a new script to a fleet. The name's extension picks the interpreter: '.sh' for macOS/Linux, '.ps1' for Windows. Omit `fleet` to save it at the Unassigned scope. This only stores the script — it does not run it on any host. CONFIRM the fleet, name, and full body with the operator before calling."),
		mcp.WithString("name", mcp.Required(), mcp.Description("Script file name including extension, e.g. 'restart-agent.sh' or 'clear-cache.ps1'.")),
		mcp.WithString("contents", mcp.Required(), mcp.Description("Full script body.")),
		mcp.WithString("fleet", mcp.Description("Fleet name to save the script under. Omit for the Unassigned scope.")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
	)
	s.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		logrus.Info("Tool invoked: create_script")

		name := strings.TrimSpace(getOptionalString(request, "name"))
		if name == "" {
			return mcp.NewToolResultError("name is required"), nil
		}
		contents := getOptionalString(request, "contents")
		if strings.TrimSpace(contents) == "" {
			return mcp.NewToolResultError("contents is required"), nil
		}

		teamID, err := fleetClient.resolveFleetID(ctx, getOptionalString(request, "fleet"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		scriptID, err := fleetClient.CreateScript(ctx, teamID, name, contents)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to create script: %v", err)), nil
		}
		return jsonResult(map[string]interface{}{"script_id": scriptID, "name": name, "fleet_id": teamID})
	})
}

func registerUpdateScript(s *server.MCPServer, fleetClient *FleetClient) {
	tool := mcp.NewTool("update_script",
		mcp.WithDescription("Replace a saved script's body. The script keeps its name and fleet — Fleet has no route for renaming a script or moving it between fleets; delete and re-create for that. Read the current body with get_script(contents='true') and CONFIRM the replacement with the operator before calling."),
		mcp.WithString("script_id", mcp.Required(), mcp.Description("Numeric script ID (from get_scripts).")),
		mcp.WithString("contents", mcp.Required(), mcp.Description("Full replacement script body.")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(true),
	)
	s.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		logrus.Info("Tool invoked: update_script")

		scriptID, err := parsePositiveUintString("script_id", getOptionalString(request, "script_id"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		contents := getOptionalString(request, "contents")
		if strings.TrimSpace(contents) == "" {
			return mcp.NewToolResultError("contents is required"), nil
		}

		// Fleet infers the interpreter from the uploaded file name, so reuse the
		// stored name rather than letting the upload rename the script.
		existing, err := fleetClient.GetScript(ctx, uint(scriptID), false)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to get script: %v", err)), nil
		}

		script, err := fleetClient.UpdateScript(ctx, uint(scriptID), existing.Name, contents)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to update script: %v", err)), nil
		}
		return jsonResult(script)
	})
}

func registerDeleteScript(s *server.MCPServer, fleetClient *FleetClient) {
	tool := mcp.NewTool("delete_script",
		mcp.WithDescription("Delete a saved script by ID. Irreversible. Show the operator the script's name and fleet (from get_script) and get explicit confirmation before calling."),
		mcp.WithString("script_id", mcp.Required(), mcp.Description("Numeric script ID (from get_scripts).")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
	)
	s.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		logrus.Info("Tool invoked: delete_script")

		scriptID, err := parsePositiveUintString("script_id", getOptionalString(request, "script_id"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		if err := fleetClient.DeleteScript(ctx, uint(scriptID)); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to delete script: %v", err)), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("Deleted script %d.", scriptID)), nil
	})
}

func registerRunScript(s *server.MCPServer, fleetClient *FleetClient) {
	tool := mcp.NewTool("run_script",
		mcp.WithDescription("Run a script on ONE host. This executes code on a managed device with the agent's privileges — the most consequential tool on this server.\n\nTwo modes: pass `script_id` to run a saved script (Fleet requires the script and the host to be in the same fleet), or `contents` to run an anonymous one-off. Exactly one of the two.\n\nBy default this waits for the host to report back and returns exit_code plus output. An offline host, or one slower than Fleet's sync timeout, comes back with host_timeout — the execution is still queued, so poll get_script_result with the returned execution_id. Pass wait='false' to queue without waiting.\n\nBEFORE CALLING: resolve the host with get_host so you are certain which machine this is, show the operator the resolved hostname AND the full script body (read a saved script with get_script(contents='true')), and get explicit confirmation. Never run a script the operator has not seen. Never infer a host from a partial name without confirming."),
		mcp.WithString("host_id", mcp.Required(), mcp.Description("Numeric Fleet host ID. Required and deliberately not a hostname — resolve the host with get_host first so the target is unambiguous.")),
		mcp.WithString("script_id", mcp.Description("Numeric saved-script ID (from get_scripts). Mutually exclusive with `contents`.")),
		mcp.WithString("contents", mcp.Description("Script body to run once without saving it. Mutually exclusive with `script_id`. The `name` argument decides the interpreter.")),
		mcp.WithString("name", mcp.Description("File name for an anonymous script, e.g. 'check.sh' or 'check.ps1'. Fleet picks the interpreter from the extension. Defaults to 'script.sh'. Ignored when `script_id` is set.")),
		mcp.WithString("fleet", mcp.Description("Fleet name that scopes the authorization check for an anonymous script. Ignored when `script_id` is set — the saved script carries its own fleet.")),
		mcp.WithString("wait", mcp.Description("'false' to queue the run and return immediately. Defaults to waiting for the result.")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
	)
	s.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		logrus.Info("Tool invoked: run_script")

		hostID, err := parsePositiveUintString("host_id", getOptionalString(request, "host_id"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		scriptIDArg := strings.TrimSpace(getOptionalString(request, "script_id"))
		contents := getOptionalString(request, "contents")
		hasContents := strings.TrimSpace(contents) != ""

		switch {
		case scriptIDArg != "" && hasContents:
			return mcp.NewToolResultError("pass either script_id or contents, not both"), nil
		case scriptIDArg == "" && !hasContents:
			return mcp.NewToolResultError("one of script_id or contents is required"), nil
		}

		var (
			scriptID *uint
			name     string
			teamID   *uint
		)
		if scriptIDArg != "" {
			parsed, err := parsePositiveUintString("script_id", scriptIDArg)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			id := uint(parsed)
			scriptID = &id
		} else {
			name = strings.TrimSpace(getOptionalString(request, "name"))
			if name == "" {
				name = "script.sh"
			}
			teamID, err = fleetClient.resolveFleetID(ctx, getOptionalString(request, "fleet"))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
		}

		wait := !strings.EqualFold(strings.TrimSpace(getOptionalString(request, "wait")), "false")

		run, err := fleetClient.RunScript(ctx, uint(hostID), scriptID, contents, name, teamID, wait)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to run script: %v", err)), nil
		}
		return jsonResult(run)
	})
}

func registerGetScriptResult(s *server.MCPServer, fleetClient *FleetClient) {
	tool := mcp.NewTool("get_script_result",
		mcp.WithDescription("Get the result of a script execution by its execution ID. A null exit_code means the host has not reported back yet — it is offline or still running; poll again rather than treating it as a failure. exit_code 0 is success, -1 means the script did not terminate normally, anything else is the script's own status."),
		mcp.WithString("execution_id", mcp.Required(), mcp.Description("Execution ID returned by run_script.")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
	)
	s.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		logrus.Info("Tool invoked: get_script_result")

		executionID := strings.TrimSpace(getOptionalString(request, "execution_id"))
		if executionID == "" {
			return mcp.NewToolResultError("execution_id is required"), nil
		}

		run, err := fleetClient.GetScriptResult(ctx, executionID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to get script result: %v", err)), nil
		}
		return jsonResult(run)
	})
}

func registerRunScriptBatch(s *server.MCPServer, fleetClient *FleetClient) {
	tool := mcp.NewTool("run_script_batch",
		mcp.WithDescription("Run a saved script across MANY hosts. Targets resolve exactly like prepare_live_query — combine fleet / platform / label / status / query / host_ids — and the tool reports how many hosts matched before queueing. Always queued, never synchronous: poll get_script_batch_status with the returned batch_execution_id.\n\nThis executes code on every matched device. BEFORE CALLING: run prepare_live_query with the same filters to preview the exact host set, show the operator that count and the full script body (get_script(contents='true')), and get explicit confirmation of both. A filter typo here hits the whole inventory."),
		mcp.WithString("script_id", mcp.Required(), mcp.Description("Numeric saved-script ID (from get_scripts). Anonymous scripts are not supported on this route — save the script first.")),
		mcp.WithString("fleet", mcp.Description("Fleet name to target.")),
		mcp.WithString("platform", mcp.Description("Platform: 'macos' / 'windows' / 'linux' / 'chromeos'.")),
		mcp.WithString("label", mcp.Description("Custom Fleet label name. Takes precedence over platform when both set.")),
		mcp.WithString("status", mcp.Description("Host status: 'online' / 'offline' / 'new' / 'mia'.")),
		mcp.WithString("query", mcp.Description("Substring matched against hostname / serial / IP / model / user inventory.")),
		mcp.WithString("host_ids", mcp.Description("Comma-separated numeric host IDs to target explicitly.")),
		mcp.WithString("hostnames", mcp.Description("Comma-separated hostnames. Errors on collision — use host_ids instead.")),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithIdempotentHintAnnotation(false),
	)
	s.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		logrus.Info("Tool invoked: run_script_batch")

		scriptID, err := parsePositiveUintString("script_id", getOptionalString(request, "script_id"))
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		spec, err := buildLiveQuerySpecFromRequest(request)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		targets, err := fleetClient.ResolveLiveQueryTargets(ctx, spec)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Target resolution failed: %v", err)), nil
		}
		if len(targets) == 0 {
			return mcp.NewToolResultError("Targets resolved to 0 hosts — refine your filters."), nil
		}

		hostIDs := make([]uint, 0, len(targets))
		for _, t := range targets {
			hostIDs = append(hostIDs, t.ID)
		}

		batchID, err := fleetClient.RunScriptBatch(ctx, uint(scriptID), hostIDs)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to run script batch: %v", err)), nil
		}
		return jsonResult(map[string]interface{}{
			"batch_execution_id":  batchID,
			"targeted_host_count": len(hostIDs),
			"script_id":           scriptID,
		})
	})
}

func registerGetScriptBatchStatus(s *server.MCPServer, fleetClient *FleetClient) {
	tool := mcp.NewTool("get_script_batch_status",
		mcp.WithDescription("Get a batch script run's progress: targeted / pending / ran / errored / canceled / incompatible host counts. 'incompatible' means the host's platform does not match the script's interpreter, not a failure of the script itself."),
		mcp.WithString("batch_execution_id", mcp.Required(), mcp.Description("Batch execution ID returned by run_script_batch.")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
	)
	s.AddTool(tool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		logrus.Info("Tool invoked: get_script_batch_status")

		batchID := strings.TrimSpace(getOptionalString(request, "batch_execution_id"))
		if batchID == "" {
			return mcp.NewToolResultError("batch_execution_id is required"), nil
		}

		status, err := fleetClient.GetScriptBatchStatus(ctx, batchID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("Failed to get batch status: %v", err)), nil
		}
		return jsonResult(status)
	})
}
