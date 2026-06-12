package v1

import (
	"testing"

	"github.com/stretchr/testify/require"

	storepb "github.com/usememos/memos/proto/gen/store"
	v1pb "github.com/usememos/memos/proto/gen/api/v1"
	"github.com/usememos/memos/store"
)

// TestConvertUserWebhookFromUserSettingOmitsSigningSecret guards the core security
// invariant of the signing-secret feature: the secret is INPUT_ONLY and must never
// be copied into an API response, even though it is persisted in the user setting.
func TestConvertUserWebhookFromUserSettingOmitsSigningSecret(t *testing.T) {
	user := &store.User{Username: "alice"}
	stored := &storepb.WebhooksUserSetting_Webhook{
		Id:            "webhook-id",
		Title:         "My Webhook",
		Url:           "https://example.com/postreceive",
		SigningSecret: "whsec_super-secret-value",
	}

	apiWebhook := convertUserWebhookFromUserSetting(stored, user)

	require.Equal(t, "My Webhook", apiWebhook.DisplayName)
	require.Equal(t, "https://example.com/postreceive", apiWebhook.Url)
	require.Empty(t, apiWebhook.SigningSecret, "signing secret must never be returned in API responses")
}

// TestConvertUserWebhookFromUserSetting_FieldMapping verifies the canonical
// field mapping between the store and API representations. This is the single
// conversion used by both the dedicated webhook CRUD endpoints and the user
// settings read path.
func TestConvertUserWebhookFromUserSetting_FieldMapping(t *testing.T) {
	user := &store.User{Username: "bob"}
	stored := &storepb.WebhooksUserSetting_Webhook{
		Id:            "abc123def456",
		Title:         "Deploy Hook",
		Url:           "https://hooks.example.com/deploy",
		SigningSecret: "whsec_secret-not-returned",
	}

	got := convertUserWebhookFromUserSetting(stored, user)

	require.Equal(t, "users/bob/webhooks/abc123def456", got.Name,
		"resource name must follow users/{username}/webhooks/{id} format")
	require.Equal(t, "Deploy Hook", got.DisplayName,
		"store Title must map to API DisplayName")
	require.Equal(t, "https://hooks.example.com/deploy", got.Url,
		"store Url must map to API Url")
	require.Empty(t, got.SigningSecret,
		"SigningSecret must never appear in API responses (INPUT_ONLY)")
	require.Empty(t, got.CreateTime,
		"CreateTime is not stored in the user-setting blob")
	require.Empty(t, got.UpdateTime,
		"UpdateTime is not stored in the user-setting blob")
}

// TestConvertUserSettingFromStore_OmitsSigningSecret verifies that the user
// settings read path (which now delegates to the shared conversion function)
// also never leaks signing secrets.
func TestConvertUserSettingFromStore_OmitsSigningSecret(t *testing.T) {
	user := &store.User{Username: "alice"}
	storeSetting := &storepb.UserSetting{
		UserId: 1,
		Key:    storepb.UserSetting_WEBHOOKS,
		Value: &storepb.UserSetting_Webhooks{
			Webhooks: &storepb.WebhooksUserSetting{
				Webhooks: []*storepb.WebhooksUserSetting_Webhook{
					{
						Id:            "hook-1",
						Title:         "Hook One",
						Url:           "https://example.com/1",
						SigningSecret: "whsec_should-not-leak-1",
					},
					{
						Id:            "hook-2",
						Title:         "Hook Two",
						Url:           "https://example.com/2",
						SigningSecret: "whsec_should-not-leak-2",
					},
				},
			},
		},
	}

	apiSetting := convertUserSettingFromStore(storeSetting, user, storepb.UserSetting_WEBHOOKS)

	require.NotNil(t, apiSetting)
	webhooksSetting := apiSetting.GetWebhooksSetting()
	require.NotNil(t, webhooksSetting, "expected WebhooksSetting value")
	require.Len(t, webhooksSetting.Webhooks, 2)

	for i, wh := range webhooksSetting.Webhooks {
		require.Empty(t, wh.SigningSecret,
			"webhook[%d] signing secret must not appear in user settings response", i)
	}

	// Verify field mapping is correct through this path too.
	require.Equal(t, "users/alice/webhooks/hook-1", webhooksSetting.Webhooks[0].Name)
	require.Equal(t, "Hook One", webhooksSetting.Webhooks[0].DisplayName)
	require.Equal(t, "https://example.com/1", webhooksSetting.Webhooks[0].Url)
	require.Equal(t, "users/alice/webhooks/hook-2", webhooksSetting.Webhooks[1].Name)
}

// TestConvertUserSettingFromStore_UsesSharedConversion ensures that the user
// settings path produces the same result as calling the shared conversion
// function directly. If these diverge, a field added in one place but not the
// other would silently break.
func TestConvertUserSettingFromStore_UsesSharedConversion(t *testing.T) {
	user := &store.User{Username: "charlie"}
	stored := &storepb.WebhooksUserSetting_Webhook{
		Id:            "hook-xyz",
		Title:         "Shared Check",
		Url:           "https://example.com/shared",
		SigningSecret: "whsec_shared-check",
	}

	// Via the dedicated conversion function.
	direct := convertUserWebhookFromUserSetting(stored, user)

	// Via the user settings read path.
	storeSetting := &storepb.UserSetting{
		UserId: 1,
		Key:    storepb.UserSetting_WEBHOOKS,
		Value: &storepb.UserSetting_Webhooks{
			Webhooks: &storepb.WebhooksUserSetting{
				Webhooks: []*storepb.WebhooksUserSetting_Webhook{stored},
			},
		},
	}
	viaSettings := convertUserSettingFromStore(storeSetting, user, storepb.UserSetting_WEBHOOKS)
	fromSettings := viaSettings.GetWebhooksSetting().Webhooks[0]

	require.Equal(t, direct.Name, fromSettings.Name)
	require.Equal(t, direct.DisplayName, fromSettings.DisplayName)
	require.Equal(t, direct.Url, fromSettings.Url)
	require.Equal(t, direct.SigningSecret, fromSettings.SigningSecret,
		"both paths must produce identical SigningSecret (both empty)")
}

// TestExtractWebhookIDFromName validates the single resource-name parser used
// by both the dedicated webhook endpoints and the user-settings conversion.
func TestExtractWebhookIDFromName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantID   string
		wantFail bool
	}{
		{
			name:   "valid resource name",
			input:  "users/alice/webhooks/abc123def456",
			wantID: "abc123def456",
		},
		{
			name:   "valid with numeric username",
			input:  "users/123/webhooks/hook-id",
			wantID: "hook-id",
		},
		{
			name:     "too few segments",
			input:    "users/alice/webhooks",
			wantFail: true,
		},
		{
			name:     "too many segments",
			input:    "users/alice/webhooks/id/extra",
			wantFail: true,
		},
		{
			name:     "wrong prefix",
			input:    "projects/alice/webhooks/id",
			wantFail: true,
		},
		{
			name:     "wrong middle segment",
			input:    "users/alice/hooks/id",
			wantFail: true,
		},
		{
			name:     "empty webhook ID",
			input:    "users/alice/webhooks/",
			wantFail: true,
		},
		{
			name:     "empty string",
			input:    "",
			wantFail: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := extractWebhookIDFromName(tt.input)
			if tt.wantFail {
				require.Error(t, err, "extractWebhookIDFromName(%q) should fail", tt.input)
				require.Empty(t, id)
			} else {
				require.NoError(t, err, "extractWebhookIDFromName(%q) should succeed", tt.input)
				require.Equal(t, tt.wantID, id)
			}
		})
	}
}

// TestConvertUserSettingToStore_PreservesSigningSecret guards against a latent
// data-loss bug: the write-side conversion must persist the signing secret so
// that a full-replace via user settings does not silently wipe secrets.
func TestConvertUserSettingToStore_PreservesSigningSecret(t *testing.T) {
	apiSetting := &v1pb.UserSetting{
		Name: "users/alice/settings/WEBHOOKS",
		Value: &v1pb.UserSetting_WebhooksSetting_{
			WebhooksSetting: &v1pb.UserSetting_WebhooksSetting{
				Webhooks: []*v1pb.UserWebhook{
					{
						Name:          "users/alice/webhooks/hook-1",
						DisplayName:   "Hook One",
						Url:           "https://example.com/1",
						SigningSecret: "whsec_must-be-preserved",
					},
					{
						Name:          "users/alice/webhooks/hook-2",
						DisplayName:   "Hook Two",
						Url:           "https://example.com/2",
						SigningSecret: "",
					},
				},
			},
		},
	}

	storeSetting, err := convertUserSettingToStore(apiSetting, 1, storepb.UserSetting_WEBHOOKS)
	require.NoError(t, err)
	require.NotNil(t, storeSetting)

	webhooks := storeSetting.GetWebhooks()
	require.NotNil(t, webhooks)
	require.Len(t, webhooks.Webhooks, 2)

	// The signing secret from the first webhook must be preserved.
	require.Equal(t, "hook-1", webhooks.Webhooks[0].Id)
	require.Equal(t, "Hook One", webhooks.Webhooks[0].Title)
	require.Equal(t, "https://example.com/1", webhooks.Webhooks[0].Url)
	require.Equal(t, "whsec_must-be-preserved", webhooks.Webhooks[0].SigningSecret,
		"signing secret must survive the API→store conversion to prevent silent data loss")

	// Empty secret stays empty.
	require.Equal(t, "hook-2", webhooks.Webhooks[1].Id)
	require.Equal(t, "", webhooks.Webhooks[1].SigningSecret)
}

// TestConvertUserSettingToStore_InvalidWebhookName verifies that a malformed
// resource name is rejected rather than silently producing an empty webhook ID.
func TestConvertUserSettingToStore_InvalidWebhookName(t *testing.T) {
	apiSetting := &v1pb.UserSetting{
		Name: "users/alice/settings/WEBHOOKS",
		Value: &v1pb.UserSetting_WebhooksSetting_{
			WebhooksSetting: &v1pb.UserSetting_WebhooksSetting{
				Webhooks: []*v1pb.UserWebhook{
					{
						Name:        "invalid/format",
						DisplayName: "Bad Hook",
						Url:         "https://example.com/bad",
					},
				},
			},
		},
	}

	_, err := convertUserSettingToStore(apiSetting, 1, storepb.UserSetting_WEBHOOKS)
	require.Error(t, err, "invalid webhook resource name must be rejected")
}

// TestConvertUserSettingToStore_FieldMapping verifies the API→store field
// mapping is the inverse of the store→API mapping.
func TestConvertUserSettingToStore_FieldMapping(t *testing.T) {
	apiSetting := &v1pb.UserSetting{
		Name: "users/bob/settings/WEBHOOKS",
		Value: &v1pb.UserSetting_WebhooksSetting_{
			WebhooksSetting: &v1pb.UserSetting_WebhooksSetting{
				Webhooks: []*v1pb.UserWebhook{
					{
						Name:          "users/bob/webhooks/abc123",
						DisplayName:   "Deploy Hook",
						Url:           "https://hooks.example.com/deploy",
						SigningSecret: "whsec_round-trip",
					},
				},
			},
		},
	}

	storeSetting, err := convertUserSettingToStore(apiSetting, 42, storepb.UserSetting_WEBHOOKS)
	require.NoError(t, err)

	webhooks := storeSetting.GetWebhooks()
	require.Len(t, webhooks.Webhooks, 1)

	got := webhooks.Webhooks[0]
	require.Equal(t, "abc123", got.Id,
		"webhook ID must be extracted from the resource name")
	require.Equal(t, "Deploy Hook", got.Title,
		"API DisplayName must map to store Title")
	require.Equal(t, "https://hooks.example.com/deploy", got.Url,
		"API Url must map to store Url")
	require.Equal(t, "whsec_round-trip", got.SigningSecret,
		"API SigningSecret must be persisted to store")
}
