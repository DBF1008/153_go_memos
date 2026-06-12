package mcp

import (
	"encoding/json"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestCuratedOperationIDsStayMemoFocused(t *testing.T) {
	require.Len(t, curatedOperationIDs, 24)

	for _, operationID := range curatedOperationIDs {
		require.NotContains(t, operationID, "Admin")
		require.NotContains(t, operationID, "AuthService_")
		require.NotContains(t, operationID, "AIService_")
		require.NotContains(t, operationID, "IdentityProviderService_")
		require.NotContains(t, operationID, "InstanceService_")
		require.NotContains(t, operationID, "PersonalAccessToken")
		require.NotContains(t, operationID, "PAT")
		require.NotContains(t, operationID, "Share")
		require.NotContains(t, operationID, "BatchDelete")
		require.NotContains(t, operationID, "Transcribe")
		require.NotContains(t, operationID, "LinkedIdentity")
		require.NotContains(t, operationID, "UserSetting")
	}

	// Notification and webhook operations are now included.
	notificationAndWebhookIDs := []string{
		"UserService_ListUserNotifications",
		"UserService_DeleteUserNotification",
		"UserService_UpdateUserNotification",
		"UserService_ListUserWebhooks",
		"UserService_CreateUserWebhook",
		"UserService_UpdateUserWebhook",
		"UserService_DeleteUserWebhook",
	}
	for _, operationID := range notificationAndWebhookIDs {
		require.Contains(t, curatedOperationIDs, operationID)
	}

	// Non-notification/webhook UserService operations remain excluded.
	excludedUserServiceIDs := []string{
		"UserService_ListUsers",
		"UserService_CreateUser",
		"UserService_GetUser",
		"UserService_DeleteUser",
		"UserService_UpdateUser",
	}
	for _, operationID := range excludedUserServiceIDs {
		require.NotContains(t, curatedOperationIDs, operationID)
	}
}

func TestToolNameFromOperationID(t *testing.T) {
	require.Equal(t, "memo_list_memos", toolNameFromOperationID("MemoService_ListMemos"))
	require.Equal(t, "attachment_get_attachment", toolNameFromOperationID("AttachmentService_GetAttachment"))
	require.Equal(t, "user_list_user_notifications", toolNameFromOperationID("UserService_ListUserNotifications"))
	require.Equal(t, "user_delete_user_notification", toolNameFromOperationID("UserService_DeleteUserNotification"))
	require.Equal(t, "user_update_user_notification", toolNameFromOperationID("UserService_UpdateUserNotification"))
	require.Equal(t, "user_list_user_webhooks", toolNameFromOperationID("UserService_ListUserWebhooks"))
	require.Equal(t, "user_create_user_webhook", toolNameFromOperationID("UserService_CreateUserWebhook"))
	require.Equal(t, "user_update_user_webhook", toolNameFromOperationID("UserService_UpdateUserWebhook"))
	require.Equal(t, "user_delete_user_webhook", toolNameFromOperationID("UserService_DeleteUserWebhook"))
}

func TestBuildToolFromOperationIncludesSchemasAndMetadata(t *testing.T) {
	spec, err := loadOpenAPISpec("../../../proto/gen/openapi.yaml")
	require.NoError(t, err)
	registry, err := buildOperationRegistry(spec)
	require.NoError(t, err)

	tool, operation := buildToolFromOperation(registry["MemoService_ListMemos"])
	require.Equal(t, "memo_list_memos", tool.Name)
	require.Equal(t, "Memo List Memos", tool.Title)
	require.Equal(t, "MemoService_ListMemos", operation.OperationID)
	require.Equal(t, "GET", operation.Method)
	require.Equal(t, "/api/v1/memos", operation.Path)
	require.Equal(t, "MemoService_ListMemos", tool.Meta["operationId"])
	require.Equal(t, "GET", tool.Meta["method"])
	require.Equal(t, "/api/v1/memos", tool.Meta["path"])
	require.NotEmpty(t, tool.Description)
	require.NotNil(t, tool.InputSchema)
	require.NotNil(t, tool.OutputSchema)
	require.NotNil(t, tool.Annotations)
	require.True(t, tool.Annotations.ReadOnlyHint)
	require.False(t, *tool.Annotations.DestructiveHint)
	require.True(t, tool.Annotations.IdempotentHint)
	require.False(t, *tool.Annotations.OpenWorldHint)

	inputBytes, err := json.Marshal(tool.InputSchema)
	require.NoError(t, err)
	require.Contains(t, string(inputBytes), `"pageSize"`)
	require.Contains(t, string(inputBytes), `"additionalProperties":false`)

	outputBytes, err := json.Marshal(tool.OutputSchema)
	require.NoError(t, err)
	require.Contains(t, string(outputBytes), `"memos"`)
}

func TestBuildToolFromOperationIncludesRequestBodySchema(t *testing.T) {
	spec, err := loadOpenAPISpec("../../../proto/gen/openapi.yaml")
	require.NoError(t, err)
	registry, err := buildOperationRegistry(spec)
	require.NoError(t, err)

	tool, operation := buildToolFromOperation(registry["MemoService_CreateMemo"])
	require.Equal(t, "POST", operation.Method)
	require.False(t, tool.Annotations.ReadOnlyHint)
	require.False(t, *tool.Annotations.DestructiveHint)
	require.False(t, tool.Annotations.IdempotentHint)

	input, ok := tool.InputSchema.(jsonSchema)
	require.True(t, ok)
	require.Contains(t, input["required"], "body")
	properties, ok := input["properties"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, properties, "memoId")
	require.Contains(t, properties, "body")
	body, ok := properties["body"].(jsonSchema)
	require.True(t, ok)
	require.Equal(t, "object", body["type"])
	require.Contains(t, body["properties"], "content")

	err = validateToolArguments(input, map[string]any{
		"body": map[string]any{
			"state":      "NORMAL",
			"content":    "hello",
			"visibility": "PRIVATE",
		},
	})
	require.NoError(t, err)
}

func TestBuildToolFromOperationIncludesNotificationAndWebhookSchemas(t *testing.T) {
	spec, err := loadOpenAPISpec("../../../proto/gen/openapi.yaml")
	require.NoError(t, err)
	registry, err := buildOperationRegistry(spec)
	require.NoError(t, err)

	// Notification list tool
	listNotificationsTool, listNotificationsOp := buildToolFromOperation(registry["UserService_ListUserNotifications"])
	require.Equal(t, "user_list_user_notifications", listNotificationsTool.Name)
	require.Equal(t, "UserService_ListUserNotifications", listNotificationsOp.OperationID)
	require.Equal(t, "GET", listNotificationsOp.Method)
	require.True(t, listNotificationsTool.Annotations.ReadOnlyHint)
	inputBytes, err := json.Marshal(listNotificationsTool.InputSchema)
	require.NoError(t, err)
	require.Contains(t, string(inputBytes), `"user"`)
	outputBytes, err := json.Marshal(listNotificationsTool.OutputSchema)
	require.NoError(t, err)
	require.Contains(t, string(outputBytes), `"notifications"`)

	// Webhook create tool
	createWebhookTool, createWebhookOp := buildToolFromOperation(registry["UserService_CreateUserWebhook"])
	require.Equal(t, "user_create_user_webhook", createWebhookTool.Name)
	require.Equal(t, "UserService_CreateUserWebhook", createWebhookOp.OperationID)
	require.Equal(t, "POST", createWebhookOp.Method)
	require.False(t, createWebhookTool.Annotations.ReadOnlyHint)
	webhookInput, ok := createWebhookTool.InputSchema.(jsonSchema)
	require.True(t, ok)
	require.Contains(t, webhookInput["required"], "body")
	webhookProperties, ok := webhookInput["properties"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, webhookProperties, "user")
	require.Contains(t, webhookProperties, "body")

	// Webhook delete tool
	deleteWebhookTool, deleteWebhookOp := buildToolFromOperation(registry["UserService_DeleteUserWebhook"])
	require.Equal(t, "user_delete_user_webhook", deleteWebhookTool.Name)
	require.Equal(t, "UserService_DeleteUserWebhook", deleteWebhookOp.OperationID)
	require.Equal(t, "DELETE", deleteWebhookOp.Method)
	require.True(t, *deleteWebhookTool.Annotations.DestructiveHint)
}

func TestBuildCuratedToolsHasUniqueNames(t *testing.T) {
	spec, err := loadOpenAPISpec("../../../proto/gen/openapi.yaml")
	require.NoError(t, err)
	registry, err := buildOperationRegistry(spec)
	require.NoError(t, err)

	tools, operations, err := buildCuratedTools(registry)
	require.NoError(t, err)
	require.Len(t, tools, len(curatedOperationIDs))
	require.Len(t, operations, len(curatedOperationIDs))

	names := map[string]struct{}{}
	for _, tool := range tools {
		require.IsType(t, &sdkmcp.Tool{}, tool)
		require.NotEmpty(t, tool.Name)
		require.NotContains(t, names, tool.Name)
		names[tool.Name] = struct{}{}
		require.Equal(t, tool.Name, operations[tool.Name].ToolName)

		inputBytes, err := json.Marshal(tool.InputSchema)
		require.NoError(t, err)
		require.NotContains(t, string(inputBytes), "#/components/schemas")
		outputBytes, err := json.Marshal(tool.OutputSchema)
		require.NoError(t, err)
		require.NotContains(t, string(outputBytes), "#/components/schemas")
	}
}

func TestBuildCuratedToolsRejectsMissingOperation(t *testing.T) {
	_, _, err := buildCuratedTools(map[string]*openAPIOperation{})
	require.ErrorContains(t, err, "curated OpenAPI operation")
	require.ErrorContains(t, err, "not found")
}

func TestBuildCuratedToolsRejectsDuplicateToolNames(t *testing.T) {
	registry := make(map[string]*openAPIOperation, len(curatedOperationIDs))
	for _, operationID := range curatedOperationIDs {
		registry[operationID] = &openAPIOperation{
			OperationID:    operationID,
			Description:    operationID,
			Method:         "GET",
			Path:           "/api/v1/test",
			ResponseSchema: okSchema(),
		}
	}
	registry["MemoService_ListMemos"].OperationID = "MemoService_GetMemo"

	_, _, err := buildCuratedTools(registry)
	require.ErrorContains(t, err, "duplicate MCP tool name")
}
