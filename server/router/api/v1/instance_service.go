package v1

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/lithammer/shortuuid/v4"
	"github.com/pkg/errors"
	colorpb "google.golang.org/genproto/googleapis/type/color"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	v1pb "github.com/usememos/memos/proto/gen/api/v1"
	storepb "github.com/usememos/memos/proto/gen/store"
	"github.com/usememos/memos/server/notification"
	"github.com/usememos/memos/store"
)

const (
	maxTranscriptionConfigModelLength    = 256
	maxTranscriptionConfigLanguageLength = 32
	maxTranscriptionConfigPromptLength   = 4096
	maxBatchGetInstanceSettings          = 100
)

type instanceSettingCaller struct {
	user   *store.User
	loaded bool
}

func (c *instanceSettingCaller) currentUser(ctx context.Context, service *APIV1Service) (*store.User, error) {
	if c.loaded {
		return c.user, nil
	}
	user, err := service.fetchCurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	c.user = user
	c.loaded = true
	return c.user, nil
}

// GetInstanceProfile returns the instance profile.
func (s *APIV1Service) GetInstanceProfile(ctx context.Context, _ *v1pb.GetInstanceProfileRequest) (*v1pb.InstanceProfile, error) {
	admin, err := s.GetInstanceAdmin(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get instance admin: %v", err)
	}

	instanceProfile := &v1pb.InstanceProfile{
		Version:     s.Profile.Version,
		Demo:        s.Profile.Demo,
		InstanceUrl: s.Profile.InstanceURL,
		Admin:       admin, // nil when not initialized
		Commit:      s.Profile.Commit,
	}
	return instanceProfile, nil
}

func (s *APIV1Service) GetInstanceSetting(ctx context.Context, request *v1pb.GetInstanceSettingRequest) (*v1pb.InstanceSetting, error) {
	return s.getInstanceSettingByName(ctx, request.Name, &instanceSettingCaller{})
}

// BatchGetInstanceSettings returns multiple instance settings in request order.
func (s *APIV1Service) BatchGetInstanceSettings(ctx context.Context, request *v1pb.BatchGetInstanceSettingsRequest) (*v1pb.BatchGetInstanceSettingsResponse, error) {
	if len(request.Names) > maxBatchGetInstanceSettings {
		return nil, status.Errorf(codes.InvalidArgument, "too many instance setting names (max %d)", maxBatchGetInstanceSettings)
	}

	caller := &instanceSettingCaller{}
	settings := make([]*v1pb.InstanceSetting, 0, len(request.Names))
	for _, name := range request.Names {
		setting, err := s.getInstanceSettingByName(ctx, name, caller)
		if err != nil {
			return nil, err
		}
		settings = append(settings, setting)
	}

	return &v1pb.BatchGetInstanceSettingsResponse{Settings: settings}, nil
}

func (s *APIV1Service) getInstanceSettingByName(ctx context.Context, name string, caller *instanceSettingCaller) (*v1pb.InstanceSetting, error) {
	instanceSettingKeyString, err := ExtractInstanceSettingKeyFromName(name)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid instance setting name: %v", err)
	}

	instanceSettingKey := storepb.InstanceSettingKey(storepb.InstanceSettingKey_value[instanceSettingKeyString])
	// Get instance setting from store with default value.
	switch instanceSettingKey {
	case storepb.InstanceSettingKey_BASIC:
		_, err = s.Store.GetInstanceBasicSetting(ctx)
	case storepb.InstanceSettingKey_GENERAL:
		_, err = s.Store.GetInstanceGeneralSetting(ctx)
	case storepb.InstanceSettingKey_MEMO_RELATED:
		_, err = s.Store.GetInstanceMemoRelatedSetting(ctx)
	case storepb.InstanceSettingKey_STORAGE:
		_, err = s.Store.GetInstanceStorageSetting(ctx)
	case storepb.InstanceSettingKey_TAGS:
		_, err = s.Store.GetInstanceTagsSetting(ctx)
	case storepb.InstanceSettingKey_NOTIFICATION:
		_, err = s.Store.GetInstanceNotificationSetting(ctx)
	case storepb.InstanceSettingKey_AI:
		_, err = s.Store.GetInstanceAISetting(ctx)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "unsupported instance setting key: %v", instanceSettingKey)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get instance setting: %v", err)
	}

	instanceSetting, err := s.Store.GetInstanceSetting(ctx, &store.FindInstanceSetting{
		Name: instanceSettingKey.String(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get instance setting: %v", err)
	}
	if instanceSetting == nil {
		return nil, status.Errorf(codes.NotFound, "instance setting not found")
	}

	// Storage and notification settings contain credentials; restrict to admins only.
	if instanceSetting.Key == storepb.InstanceSettingKey_STORAGE ||
		instanceSetting.Key == storepb.InstanceSettingKey_NOTIFICATION {
		user, err := caller.currentUser(ctx, s)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to get current user: %v", err)
		}
		if user == nil {
			return nil, status.Errorf(codes.Unauthenticated, "user not authenticated")
		}
		if user.Role != store.RoleAdmin {
			return nil, status.Errorf(codes.PermissionDenied, "permission denied")
		}
	}
	isAdminCaller := false
	if instanceSetting.Key == storepb.InstanceSettingKey_AI {
		user, err := caller.currentUser(ctx, s)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to get current user: %v", err)
		}
		if user == nil {
			return nil, status.Errorf(codes.Unauthenticated, "user not authenticated")
		}
		isAdminCaller = user.Role == store.RoleAdmin
	}

	result := convertInstanceSettingFromStore(instanceSetting)
	if instanceSetting.Key == storepb.InstanceSettingKey_AI && !isAdminCaller {
		// Non-admin callers only need transcription.provider_id to gate the
		// editor's Transcribe button. Model / language / prompt are
		// admin-entered defaults that may contain proprietary glossary terms,
		// so they are redacted from non-admin responses.
		if ai := result.GetAiSetting(); ai != nil && ai.Transcription != nil {
			ai.Transcription.Model = ""
			ai.Transcription.Language = ""
			ai.Transcription.Prompt = ""
		}
	}
	return result, nil
}

func (s *APIV1Service) UpdateInstanceSetting(ctx context.Context, request *v1pb.UpdateInstanceSettingRequest) (*v1pb.InstanceSetting, error) {
	user, err := s.fetchCurrentUser(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get current user: %v", err)
	}
	if user == nil {
		return nil, status.Errorf(codes.Unauthenticated, "user not authenticated")
	}
	if user.Role != store.RoleAdmin {
		return nil, status.Errorf(codes.PermissionDenied, "permission denied")
	}

	if err := validateInstanceSetting(request.Setting); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid instance setting: %v", err)
	}

	var updateSetting *storepb.InstanceSetting

	hasUpdateMask := request.UpdateMask != nil && len(request.UpdateMask.Paths) > 0
	if hasUpdateMask {
		merged, err := s.mergeInstanceSettingWithMask(ctx, request.Setting, request.UpdateMask.Paths)
		if err != nil {
			return nil, err
		}
		updateSetting = merged
	} else {
		// Legacy behavior: full replace with credential preservation.
		updateSetting = convertInstanceSettingToStore(request.Setting)
		if err := s.preserveCredentialsForFullReplace(ctx, updateSetting); err != nil {
			return nil, err
		}
	}

	instanceSetting, err := s.Store.UpsertInstanceSetting(ctx, updateSetting)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to upsert instance setting: %v", err)
	}

	return convertInstanceSettingFromStore(instanceSetting), nil
}

// mergeInstanceSettingWithMask loads the existing setting, clones it, and applies
// only the fields named in the update_mask paths from the incoming request.
func (s *APIV1Service) mergeInstanceSettingWithMask(ctx context.Context, setting *v1pb.InstanceSetting, paths []string) (*storepb.InstanceSetting, error) {
	settingKeyString, err := ExtractInstanceSettingKeyFromName(setting.Name)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid instance setting name: %v", err)
	}
	instanceSettingKey := storepb.InstanceSettingKey(storepb.InstanceSettingKey_value[settingKeyString])

	result := &storepb.InstanceSetting{Key: instanceSettingKey}

	switch instanceSettingKey {
	case storepb.InstanceSettingKey_GENERAL:
		merged, err := s.mergeGeneralSetting(ctx, setting.GetGeneralSetting(), paths)
		if err != nil {
			return nil, err
		}
		result.Value = &storepb.InstanceSetting_GeneralSetting{GeneralSetting: merged}

	case storepb.InstanceSettingKey_STORAGE:
		merged, err := s.mergeStorageSetting(ctx, setting.GetStorageSetting(), paths)
		if err != nil {
			return nil, err
		}
		result.Value = &storepb.InstanceSetting_StorageSetting{StorageSetting: merged}

	case storepb.InstanceSettingKey_MEMO_RELATED:
		merged, err := s.mergeMemoRelatedSetting(ctx, setting.GetMemoRelatedSetting(), paths)
		if err != nil {
			return nil, err
		}
		result.Value = &storepb.InstanceSetting_MemoRelatedSetting{MemoRelatedSetting: merged}

	case storepb.InstanceSettingKey_TAGS:
		merged, err := s.mergeTagsSetting(ctx, setting.GetTagsSetting(), paths)
		if err != nil {
			return nil, err
		}
		result.Value = &storepb.InstanceSetting_TagsSetting{TagsSetting: merged}

	case storepb.InstanceSettingKey_NOTIFICATION:
		merged, err := s.mergeNotificationSetting(ctx, setting.GetNotificationSetting(), paths)
		if err != nil {
			return nil, err
		}
		result.Value = &storepb.InstanceSetting_NotificationSetting{NotificationSetting: merged}

	case storepb.InstanceSettingKey_AI:
		merged, err := s.mergeAISetting(ctx, setting.GetAiSetting(), paths)
		if err != nil {
			return nil, err
		}
		result.Value = &storepb.InstanceSetting_AiSetting{AiSetting: merged}

	default:
		return nil, status.Errorf(codes.InvalidArgument, "unsupported instance setting key: %v", instanceSettingKey)
	}

	return result, nil
}

// preserveCredentialsForFullReplace preserves write-only credential fields when
// the caller sends an empty value during a full (no-mask) replace.
func (s *APIV1Service) preserveCredentialsForFullReplace(ctx context.Context, updateSetting *storepb.InstanceSetting) error {
	switch updateSetting.Key {
	case storepb.InstanceSettingKey_NOTIFICATION:
		if notif := updateSetting.GetNotificationSetting(); notif != nil && notif.Email != nil && notif.Email.SmtpPassword == "" {
			existing, err := s.Store.GetInstanceNotificationSetting(ctx)
			if err == nil && existing != nil && existing.Email != nil {
				if existing.Email.SmtpPassword != "" && !sameSMTPConnectionIdentity(notif.Email, existing.Email) {
					return status.Errorf(codes.InvalidArgument, "smtp password is required when changing SMTP host, port, username, or encryption settings")
				}
				notif.Email.SmtpPassword = existing.Email.SmtpPassword
			}
		}
	case storepb.InstanceSettingKey_STORAGE:
		if storage := updateSetting.GetStorageSetting(); storage != nil && storage.S3Config != nil && storage.S3Config.AccessKeySecret == "" {
			existing, err := s.Store.GetInstanceStorageSetting(ctx)
			if err == nil && existing != nil && existing.S3Config != nil {
				storage.S3Config.AccessKeySecret = existing.S3Config.AccessKeySecret
			}
		}
	case storepb.InstanceSettingKey_AI:
		if err := s.prepareInstanceAISettingForUpdate(ctx, updateSetting.GetAiSetting()); err != nil {
			return status.Errorf(codes.InvalidArgument, "invalid AI setting: %v", err)
		}
	default:
		// No credential preservation needed for other setting types.
	}
	return nil
}

// mergeGeneralSetting merges the incoming general setting into the existing one,
// applying only the fields named in paths.
func (s *APIV1Service) mergeGeneralSetting(ctx context.Context, incoming *v1pb.InstanceSetting_GeneralSetting, paths []string) (*storepb.InstanceGeneralSetting, error) {
	existing, err := s.Store.GetInstanceGeneralSetting(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get existing general setting: %v", err)
	}
	merged := proto.Clone(existing).(*storepb.InstanceGeneralSetting)

	if incoming == nil {
		return merged, nil
	}

	for _, path := range paths {
		switch path {
		case "general_setting":
			merged = convertInstanceGeneralSettingToStore(incoming)
		case "general_setting.disallow_user_registration":
			merged.DisallowUserRegistration = incoming.DisallowUserRegistration
		case "general_setting.disallow_password_auth":
			merged.DisallowPasswordAuth = incoming.DisallowPasswordAuth
		case "general_setting.additional_script":
			merged.AdditionalScript = incoming.AdditionalScript
		case "general_setting.additional_style":
			merged.AdditionalStyle = incoming.AdditionalStyle
		case "general_setting.custom_profile":
			if incoming.CustomProfile != nil {
				merged.CustomProfile = &storepb.InstanceCustomProfile{
					Title:       incoming.CustomProfile.Title,
					Description: incoming.CustomProfile.Description,
					LogoUrl:     incoming.CustomProfile.LogoUrl,
				}
			} else {
				merged.CustomProfile = nil
			}
		case "general_setting.week_start_day_offset":
			merged.WeekStartDayOffset = incoming.WeekStartDayOffset
		case "general_setting.disallow_change_username":
			merged.DisallowChangeUsername = incoming.DisallowChangeUsername
		case "general_setting.disallow_change_nickname":
			merged.DisallowChangeNickname = incoming.DisallowChangeNickname
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update mask path for general setting: %s", path)
		}
	}
	return merged, nil
}

// mergeStorageSetting merges the incoming storage setting into the existing one,
// applying only the fields named in paths. S3 access key secret is preserved
// when the incoming value is empty.
func (s *APIV1Service) mergeStorageSetting(ctx context.Context, incoming *v1pb.InstanceSetting_StorageSetting, paths []string) (*storepb.InstanceStorageSetting, error) {
	existing, err := s.Store.GetInstanceStorageSetting(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get existing storage setting: %v", err)
	}
	merged := proto.Clone(existing).(*storepb.InstanceStorageSetting)

	if incoming == nil {
		return merged, nil
	}

	for _, path := range paths {
		switch path {
		case "storage_setting":
			incomingStore := convertInstanceStorageSettingToStore(incoming)
			merged = incomingStore
			// Preserve S3 secret if empty.
			if merged.S3Config != nil && merged.S3Config.AccessKeySecret == "" {
				if existing.S3Config != nil {
					merged.S3Config.AccessKeySecret = existing.S3Config.AccessKeySecret
				}
			}
		case "storage_setting.storage_type":
			merged.StorageType = storepb.InstanceStorageSetting_StorageType(incoming.StorageType)
		case "storage_setting.filepath_template":
			merged.FilepathTemplate = incoming.FilepathTemplate
		case "storage_setting.upload_size_limit_mb":
			merged.UploadSizeLimitMb = incoming.UploadSizeLimitMb
		case "storage_setting.s3_config":
			if incoming.S3Config != nil {
				merged.S3Config = &storepb.StorageS3Config{
					AccessKeyId:     incoming.S3Config.AccessKeyId,
					AccessKeySecret: incoming.S3Config.AccessKeySecret,
					Endpoint:        incoming.S3Config.Endpoint,
					Region:          incoming.S3Config.Region,
					Bucket:          incoming.S3Config.Bucket,
					UsePathStyle:    incoming.S3Config.UsePathStyle,
				}
				// Preserve secret if empty.
				if merged.S3Config.AccessKeySecret == "" && existing.S3Config != nil {
					merged.S3Config.AccessKeySecret = existing.S3Config.AccessKeySecret
				}
			} else {
				merged.S3Config = nil
			}
		case "storage_setting.s3_config.access_key_id":
			if merged.S3Config == nil {
				merged.S3Config = &storepb.StorageS3Config{}
			}
			merged.S3Config.AccessKeyId = incoming.GetS3Config().GetAccessKeyId()
		case "storage_setting.s3_config.access_key_secret":
			if merged.S3Config == nil {
				merged.S3Config = &storepb.StorageS3Config{}
			}
			secret := incoming.GetS3Config().GetAccessKeySecret()
			if secret == "" && existing.S3Config != nil {
				secret = existing.S3Config.AccessKeySecret
			}
			merged.S3Config.AccessKeySecret = secret
		case "storage_setting.s3_config.endpoint":
			if merged.S3Config == nil {
				merged.S3Config = &storepb.StorageS3Config{}
			}
			merged.S3Config.Endpoint = incoming.GetS3Config().GetEndpoint()
		case "storage_setting.s3_config.region":
			if merged.S3Config == nil {
				merged.S3Config = &storepb.StorageS3Config{}
			}
			merged.S3Config.Region = incoming.GetS3Config().GetRegion()
		case "storage_setting.s3_config.bucket":
			if merged.S3Config == nil {
				merged.S3Config = &storepb.StorageS3Config{}
			}
			merged.S3Config.Bucket = incoming.GetS3Config().GetBucket()
		case "storage_setting.s3_config.use_path_style":
			if merged.S3Config == nil {
				merged.S3Config = &storepb.StorageS3Config{}
			}
			merged.S3Config.UsePathStyle = incoming.GetS3Config().GetUsePathStyle()
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update mask path for storage setting: %s", path)
		}
	}
	return merged, nil
}

// mergeMemoRelatedSetting merges the incoming memo-related setting into the
// existing one, applying only the fields named in paths.
func (s *APIV1Service) mergeMemoRelatedSetting(ctx context.Context, incoming *v1pb.InstanceSetting_MemoRelatedSetting, paths []string) (*storepb.InstanceMemoRelatedSetting, error) {
	existing, err := s.Store.GetInstanceMemoRelatedSetting(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get existing memo related setting: %v", err)
	}
	merged := proto.Clone(existing).(*storepb.InstanceMemoRelatedSetting)

	if incoming == nil {
		return merged, nil
	}

	for _, path := range paths {
		switch path {
		case "memo_related_setting":
			merged = convertInstanceMemoRelatedSettingToStore(incoming)
		case "memo_related_setting.content_length_limit":
			merged.ContentLengthLimit = incoming.ContentLengthLimit
		case "memo_related_setting.enable_double_click_edit":
			merged.EnableDoubleClickEdit = incoming.EnableDoubleClickEdit
		case "memo_related_setting.reactions":
			merged.Reactions = incoming.Reactions
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update mask path for memo related setting: %s", path)
		}
	}
	return merged, nil
}

// mergeTagsSetting merges the incoming tags setting into the existing one,
// applying only the fields named in paths. Accepts both "tags_setting" and
// "tags" for backward compatibility.
func (s *APIV1Service) mergeTagsSetting(ctx context.Context, incoming *v1pb.InstanceSetting_TagsSetting, paths []string) (*storepb.InstanceTagsSetting, error) {
	existing, err := s.Store.GetInstanceTagsSetting(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get existing tags setting: %v", err)
	}
	merged := proto.Clone(existing).(*storepb.InstanceTagsSetting)

	if incoming == nil {
		return merged, nil
	}

	for _, path := range paths {
		switch path {
		case "tags_setting", "tags":
			// Full replace of tags map.
			converted := convertInstanceTagsSettingToStore(incoming)
			merged.Tags = converted.Tags
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update mask path for tags setting: %s", path)
		}
	}
	return merged, nil
}

// mergeNotificationSetting merges the incoming notification setting into the
// existing one, applying only the fields named in paths. SMTP password is
// preserved when the incoming value is empty, and required when the SMTP
// connection identity changes.
func (s *APIV1Service) mergeNotificationSetting(ctx context.Context, incoming *v1pb.InstanceSetting_NotificationSetting, paths []string) (*storepb.InstanceNotificationSetting, error) {
	existing, err := s.Store.GetInstanceNotificationSetting(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get existing notification setting: %v", err)
	}
	merged := proto.Clone(existing).(*storepb.InstanceNotificationSetting)

	if incoming == nil {
		return merged, nil
	}

	for _, path := range paths {
		switch path {
		case "notification_setting":
			incomingStore := convertInstanceNotificationSettingToStore(incoming)
			merged = incomingStore
			// Preserve SMTP password if empty.
			if merged.Email != nil && merged.Email.SmtpPassword == "" {
				if existing.Email != nil {
					if existing.Email.SmtpPassword != "" && !sameSMTPConnectionIdentity(merged.Email, existing.Email) {
						return nil, status.Errorf(codes.InvalidArgument, "smtp password is required when changing SMTP host, port, username, or encryption settings")
					}
					merged.Email.SmtpPassword = existing.Email.SmtpPassword
				}
			}
		case "notification_setting.email":
			if incoming.Email != nil {
				newEmail := &storepb.InstanceNotificationSetting_EmailSetting{
					Enabled:      incoming.Email.Enabled,
					SmtpHost:     incoming.Email.SmtpHost,
					SmtpPort:     incoming.Email.SmtpPort,
					SmtpUsername: incoming.Email.SmtpUsername,
					SmtpPassword: incoming.Email.SmtpPassword,
					FromEmail:    incoming.Email.FromEmail,
					FromName:     incoming.Email.FromName,
					ReplyTo:      incoming.Email.ReplyTo,
					UseTls:       incoming.Email.UseTls,
					UseSsl:       incoming.Email.UseSsl,
				}
				// Preserve password if empty.
				if newEmail.SmtpPassword == "" && existing.Email != nil {
					if existing.Email.SmtpPassword != "" && !sameSMTPConnectionIdentity(newEmail, existing.Email) {
						return nil, status.Errorf(codes.InvalidArgument, "smtp password is required when changing SMTP host, port, username, or encryption settings")
					}
					newEmail.SmtpPassword = existing.Email.SmtpPassword
				}
				merged.Email = newEmail
			} else {
				merged.Email = nil
			}
		case "notification_setting.email.enabled":
			if merged.Email == nil {
				merged.Email = &storepb.InstanceNotificationSetting_EmailSetting{}
			}
			merged.Email.Enabled = incoming.GetEmail().GetEnabled()
		case "notification_setting.email.smtp_host":
			if merged.Email == nil {
				merged.Email = &storepb.InstanceNotificationSetting_EmailSetting{}
			}
			merged.Email.SmtpHost = incoming.GetEmail().GetSmtpHost()
		case "notification_setting.email.smtp_port":
			if merged.Email == nil {
				merged.Email = &storepb.InstanceNotificationSetting_EmailSetting{}
			}
			merged.Email.SmtpPort = incoming.GetEmail().GetSmtpPort()
		case "notification_setting.email.smtp_username":
			if merged.Email == nil {
				merged.Email = &storepb.InstanceNotificationSetting_EmailSetting{}
			}
			merged.Email.SmtpUsername = incoming.GetEmail().GetSmtpUsername()
		case "notification_setting.email.smtp_password":
			if merged.Email == nil {
				merged.Email = &storepb.InstanceNotificationSetting_EmailSetting{}
			}
			password := incoming.GetEmail().GetSmtpPassword()
			if password == "" && existing.Email != nil {
				password = existing.Email.SmtpPassword
			}
			merged.Email.SmtpPassword = password
		case "notification_setting.email.from_email":
			if merged.Email == nil {
				merged.Email = &storepb.InstanceNotificationSetting_EmailSetting{}
			}
			merged.Email.FromEmail = incoming.GetEmail().GetFromEmail()
		case "notification_setting.email.from_name":
			if merged.Email == nil {
				merged.Email = &storepb.InstanceNotificationSetting_EmailSetting{}
			}
			merged.Email.FromName = incoming.GetEmail().GetFromName()
		case "notification_setting.email.reply_to":
			if merged.Email == nil {
				merged.Email = &storepb.InstanceNotificationSetting_EmailSetting{}
			}
			merged.Email.ReplyTo = incoming.GetEmail().GetReplyTo()
		case "notification_setting.email.use_tls":
			if merged.Email == nil {
				merged.Email = &storepb.InstanceNotificationSetting_EmailSetting{}
			}
			merged.Email.UseTls = incoming.GetEmail().GetUseTls()
		case "notification_setting.email.use_ssl":
			if merged.Email == nil {
				merged.Email = &storepb.InstanceNotificationSetting_EmailSetting{}
			}
			merged.Email.UseSsl = incoming.GetEmail().GetUseSsl()
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update mask path for notification setting: %s", path)
		}
	}
	return merged, nil
}

// mergeAISetting merges the incoming AI setting into the existing one, applying
// only the fields named in paths. Provider API keys are preserved when the
// incoming value is empty, and transcription config is preserved when omitted.
func (s *APIV1Service) mergeAISetting(ctx context.Context, incoming *v1pb.InstanceSetting_AISetting, paths []string) (*storepb.InstanceAISetting, error) {
	existing, err := s.Store.GetInstanceAISetting(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get existing AI setting: %v", err)
	}
	merged := proto.Clone(existing).(*storepb.InstanceAISetting)

	if incoming == nil {
		return merged, nil
	}

	for _, path := range paths {
		switch path {
		case "ai_setting":
			incomingStore := convertInstanceAISettingToStore(incoming)
			merged = incomingStore
			// Preserve API keys for providers with empty keys.
			preserveAIAPIKeys(merged, existing)
			// Preserve transcription if omitted.
			if merged.Transcription == nil && existing.Transcription != nil {
				merged.Transcription = existing.Transcription
			}
		case "ai_setting.providers":
			merged.Providers = make([]*storepb.AIProviderConfig, 0, len(incoming.Providers))
			for _, p := range incoming.Providers {
				if p == nil {
					continue
				}
				provider := &storepb.AIProviderConfig{
					Id:       p.Id,
					Title:    p.Title,
					Type:     storepb.AIProviderType(p.Type),
					Endpoint: p.Endpoint,
					ApiKey:   p.ApiKey,
				}
				// Preserve API key if empty.
				if provider.ApiKey == "" {
					for _, ep := range existing.Providers {
						if ep != nil && ep.Id == provider.Id {
							provider.ApiKey = ep.ApiKey
							break
						}
					}
				}
				merged.Providers = append(merged.Providers, provider)
			}
		case "ai_setting.transcription":
			if incoming.Transcription != nil {
				merged.Transcription = convertTranscriptionConfigToStore(incoming.Transcription)
			} else {
				merged.Transcription = nil
			}
		case "ai_setting.transcription.provider_id":
			if merged.Transcription == nil {
				merged.Transcription = &storepb.TranscriptionConfig{}
			}
			merged.Transcription.ProviderId = incoming.GetTranscription().GetProviderId()
		case "ai_setting.transcription.model":
			if merged.Transcription == nil {
				merged.Transcription = &storepb.TranscriptionConfig{}
			}
			merged.Transcription.Model = incoming.GetTranscription().GetModel()
		case "ai_setting.transcription.language":
			if merged.Transcription == nil {
				merged.Transcription = &storepb.TranscriptionConfig{}
			}
			merged.Transcription.Language = incoming.GetTranscription().GetLanguage()
		case "ai_setting.transcription.prompt":
			if merged.Transcription == nil {
				merged.Transcription = &storepb.TranscriptionConfig{}
			}
			merged.Transcription.Prompt = incoming.GetTranscription().GetPrompt()
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unsupported update mask path for AI setting: %s", path)
		}
	}

	// Run AI-specific validation (ID generation, endpoint defaults, etc.).
	if err := s.prepareInstanceAISettingForUpdate(ctx, merged); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid AI setting: %v", err)
	}

	return merged, nil
}

// preserveAIAPIKeys copies API keys from existing providers to merged providers
// when the merged provider has an empty key.
func preserveAIAPIKeys(merged, existing *storepb.InstanceAISetting) {
	if existing == nil {
		return
	}
	existingMap := make(map[string]string)
	for _, p := range existing.Providers {
		if p != nil && p.Id != "" {
			existingMap[p.Id] = p.ApiKey
		}
	}
	for _, p := range merged.Providers {
		if p != nil && p.ApiKey == "" {
			if key, ok := existingMap[p.Id]; ok {
				p.ApiKey = key
			}
		}
	}
}

func (s *APIV1Service) TestInstanceEmailSetting(ctx context.Context, request *v1pb.TestInstanceEmailSettingRequest) (*emptypb.Empty, error) {
	user, err := s.fetchCurrentUser(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get current user: %v", err)
	}
	if user == nil {
		return nil, status.Errorf(codes.Unauthenticated, "user not authenticated")
	}
	if user.Role != store.RoleAdmin {
		return nil, status.Errorf(codes.PermissionDenied, "permission denied")
	}

	emailSetting, err := s.resolveTestEmailSetting(ctx, request.Email)
	if err != nil {
		return nil, err
	}

	recipientEmail := strings.TrimSpace(request.RecipientEmail)
	if recipientEmail == "" {
		recipientEmail = strings.TrimSpace(user.Email)
	}
	if recipientEmail == "" {
		return nil, status.Errorf(codes.InvalidArgument, "recipient email is required")
	}

	if err := notification.ValidateEmailSetting(emailSetting); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid notification email setting: %v", err)
	}

	if err := notification.SendTestEmail(emailSetting, recipientEmail); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to send test email: %v. Check that the SMTP port matches encryption: Gmail uses port 587 with STARTTLS on and SSL/TLS off; port 465 requires SSL/TLS on", err)
	}

	return &emptypb.Empty{}, nil
}

func (s *APIV1Service) resolveTestEmailSetting(ctx context.Context, requestEmail *v1pb.InstanceSetting_NotificationSetting_EmailSetting) (*storepb.InstanceNotificationSetting_EmailSetting, error) {
	if requestEmail == nil {
		existing, err := s.Store.GetInstanceNotificationSetting(ctx)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to get notification setting: %v", err)
		}
		return existing.GetEmail(), nil
	}

	emailSetting := convertInstanceNotificationSettingToStore(&v1pb.InstanceSetting_NotificationSetting{Email: requestEmail}).GetEmail()
	if emailSetting.SmtpPassword != "" {
		return emailSetting, nil
	}

	existing, err := s.Store.GetInstanceNotificationSetting(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get notification setting: %v", err)
	}
	existingEmail := existing.GetEmail()
	if existingEmail == nil || existingEmail.SmtpPassword == "" {
		return emailSetting, nil
	}
	if sameSMTPConnectionIdentity(emailSetting, existingEmail) {
		emailSetting.SmtpPassword = existingEmail.SmtpPassword
		return emailSetting, nil
	}
	return nil, status.Errorf(codes.InvalidArgument, "smtp password is required when changing SMTP host, port, username, or encryption settings")
}

func sameSMTPConnectionIdentity(setting, existing *storepb.InstanceNotificationSetting_EmailSetting) bool {
	if setting == nil || existing == nil {
		return false
	}
	return strings.TrimSpace(setting.SmtpHost) == strings.TrimSpace(existing.SmtpHost) &&
		setting.SmtpPort == existing.SmtpPort &&
		strings.TrimSpace(setting.SmtpUsername) == strings.TrimSpace(existing.SmtpUsername) &&
		setting.UseTls == existing.UseTls &&
		setting.UseSsl == existing.UseSsl
}

func convertInstanceSettingFromStore(setting *storepb.InstanceSetting) *v1pb.InstanceSetting {
	instanceSetting := &v1pb.InstanceSetting{
		Name: fmt.Sprintf("instance/settings/%s", setting.Key.String()),
	}
	switch setting.Value.(type) {
	case *storepb.InstanceSetting_GeneralSetting:
		instanceSetting.Value = &v1pb.InstanceSetting_GeneralSetting_{
			GeneralSetting: convertInstanceGeneralSettingFromStore(setting.GetGeneralSetting()),
		}
	case *storepb.InstanceSetting_StorageSetting:
		instanceSetting.Value = &v1pb.InstanceSetting_StorageSetting_{
			StorageSetting: convertInstanceStorageSettingFromStore(setting.GetStorageSetting()),
		}
	case *storepb.InstanceSetting_MemoRelatedSetting:
		instanceSetting.Value = &v1pb.InstanceSetting_MemoRelatedSetting_{
			MemoRelatedSetting: convertInstanceMemoRelatedSettingFromStore(setting.GetMemoRelatedSetting()),
		}
	case *storepb.InstanceSetting_TagsSetting:
		instanceSetting.Value = &v1pb.InstanceSetting_TagsSetting_{
			TagsSetting: convertInstanceTagsSettingFromStore(setting.GetTagsSetting()),
		}
	case *storepb.InstanceSetting_NotificationSetting:
		instanceSetting.Value = &v1pb.InstanceSetting_NotificationSetting_{
			NotificationSetting: convertInstanceNotificationSettingFromStore(setting.GetNotificationSetting()),
		}
	case *storepb.InstanceSetting_AiSetting:
		instanceSetting.Value = &v1pb.InstanceSetting_AiSetting{
			AiSetting: convertInstanceAISettingFromStore(setting.GetAiSetting()),
		}
	default:
		// Leave Value unset for unsupported setting variants.
	}
	return instanceSetting
}

func convertInstanceSettingToStore(setting *v1pb.InstanceSetting) *storepb.InstanceSetting {
	settingKeyString, _ := ExtractInstanceSettingKeyFromName(setting.Name)
	instanceSetting := &storepb.InstanceSetting{
		Key: storepb.InstanceSettingKey(storepb.InstanceSettingKey_value[settingKeyString]),
		Value: &storepb.InstanceSetting_GeneralSetting{
			GeneralSetting: convertInstanceGeneralSettingToStore(setting.GetGeneralSetting()),
		},
	}
	switch instanceSetting.Key {
	case storepb.InstanceSettingKey_GENERAL:
		instanceSetting.Value = &storepb.InstanceSetting_GeneralSetting{
			GeneralSetting: convertInstanceGeneralSettingToStore(setting.GetGeneralSetting()),
		}
	case storepb.InstanceSettingKey_STORAGE:
		instanceSetting.Value = &storepb.InstanceSetting_StorageSetting{
			StorageSetting: convertInstanceStorageSettingToStore(setting.GetStorageSetting()),
		}
	case storepb.InstanceSettingKey_MEMO_RELATED:
		instanceSetting.Value = &storepb.InstanceSetting_MemoRelatedSetting{
			MemoRelatedSetting: convertInstanceMemoRelatedSettingToStore(setting.GetMemoRelatedSetting()),
		}
	case storepb.InstanceSettingKey_TAGS:
		instanceSetting.Value = &storepb.InstanceSetting_TagsSetting{
			TagsSetting: convertInstanceTagsSettingToStore(setting.GetTagsSetting()),
		}
	case storepb.InstanceSettingKey_NOTIFICATION:
		instanceSetting.Value = &storepb.InstanceSetting_NotificationSetting{
			NotificationSetting: convertInstanceNotificationSettingToStore(setting.GetNotificationSetting()),
		}
	case storepb.InstanceSettingKey_AI:
		instanceSetting.Value = &storepb.InstanceSetting_AiSetting{
			AiSetting: convertInstanceAISettingToStore(setting.GetAiSetting()),
		}
	default:
		// Keep the default GeneralSetting value
	}
	return instanceSetting
}

func convertInstanceGeneralSettingFromStore(setting *storepb.InstanceGeneralSetting) *v1pb.InstanceSetting_GeneralSetting {
	if setting == nil {
		return nil
	}

	generalSetting := &v1pb.InstanceSetting_GeneralSetting{
		DisallowUserRegistration: setting.DisallowUserRegistration,
		DisallowPasswordAuth:     setting.DisallowPasswordAuth,
		AdditionalScript:         setting.AdditionalScript,
		AdditionalStyle:          setting.AdditionalStyle,
		WeekStartDayOffset:       setting.WeekStartDayOffset,
		DisallowChangeUsername:   setting.DisallowChangeUsername,
		DisallowChangeNickname:   setting.DisallowChangeNickname,
	}
	if setting.CustomProfile != nil {
		generalSetting.CustomProfile = &v1pb.InstanceSetting_GeneralSetting_CustomProfile{
			Title:       setting.CustomProfile.Title,
			Description: setting.CustomProfile.Description,
			LogoUrl:     setting.CustomProfile.LogoUrl,
		}
	}
	return generalSetting
}

func convertInstanceGeneralSettingToStore(setting *v1pb.InstanceSetting_GeneralSetting) *storepb.InstanceGeneralSetting {
	if setting == nil {
		return nil
	}
	generalSetting := &storepb.InstanceGeneralSetting{
		DisallowUserRegistration: setting.DisallowUserRegistration,
		DisallowPasswordAuth:     setting.DisallowPasswordAuth,
		AdditionalScript:         setting.AdditionalScript,
		AdditionalStyle:          setting.AdditionalStyle,
		WeekStartDayOffset:       setting.WeekStartDayOffset,
		DisallowChangeUsername:   setting.DisallowChangeUsername,
		DisallowChangeNickname:   setting.DisallowChangeNickname,
	}
	if setting.CustomProfile != nil {
		generalSetting.CustomProfile = &storepb.InstanceCustomProfile{
			Title:       setting.CustomProfile.Title,
			Description: setting.CustomProfile.Description,
			LogoUrl:     setting.CustomProfile.LogoUrl,
		}
	}
	return generalSetting
}

func convertInstanceStorageSettingFromStore(settingpb *storepb.InstanceStorageSetting) *v1pb.InstanceSetting_StorageSetting {
	if settingpb == nil {
		return nil
	}
	setting := &v1pb.InstanceSetting_StorageSetting{
		StorageType:       v1pb.InstanceSetting_StorageSetting_StorageType(settingpb.StorageType),
		FilepathTemplate:  settingpb.FilepathTemplate,
		UploadSizeLimitMb: settingpb.UploadSizeLimitMb,
	}
	if settingpb.S3Config != nil {
		setting.S3Config = &v1pb.InstanceSetting_StorageSetting_S3Config{
			AccessKeyId: settingpb.S3Config.AccessKeyId,
			// AccessKeySecret is write-only: never returned in responses.
			Endpoint:     settingpb.S3Config.Endpoint,
			Region:       settingpb.S3Config.Region,
			Bucket:       settingpb.S3Config.Bucket,
			UsePathStyle: settingpb.S3Config.UsePathStyle,
		}
	}
	return setting
}

func convertInstanceStorageSettingToStore(setting *v1pb.InstanceSetting_StorageSetting) *storepb.InstanceStorageSetting {
	if setting == nil {
		return nil
	}
	settingpb := &storepb.InstanceStorageSetting{
		StorageType:       storepb.InstanceStorageSetting_StorageType(setting.StorageType),
		FilepathTemplate:  setting.FilepathTemplate,
		UploadSizeLimitMb: setting.UploadSizeLimitMb,
	}
	if setting.S3Config != nil {
		settingpb.S3Config = &storepb.StorageS3Config{
			AccessKeyId:     setting.S3Config.AccessKeyId,
			AccessKeySecret: setting.S3Config.AccessKeySecret,
			Endpoint:        setting.S3Config.Endpoint,
			Region:          setting.S3Config.Region,
			Bucket:          setting.S3Config.Bucket,
			UsePathStyle:    setting.S3Config.UsePathStyle,
		}
	}
	return settingpb
}

func convertInstanceMemoRelatedSettingFromStore(setting *storepb.InstanceMemoRelatedSetting) *v1pb.InstanceSetting_MemoRelatedSetting {
	if setting == nil {
		return nil
	}
	return &v1pb.InstanceSetting_MemoRelatedSetting{
		ContentLengthLimit:    setting.ContentLengthLimit,
		EnableDoubleClickEdit: setting.EnableDoubleClickEdit,
		Reactions:             setting.Reactions,
	}
}

func convertInstanceMemoRelatedSettingToStore(setting *v1pb.InstanceSetting_MemoRelatedSetting) *storepb.InstanceMemoRelatedSetting {
	if setting == nil {
		return nil
	}
	return &storepb.InstanceMemoRelatedSetting{
		ContentLengthLimit:    setting.ContentLengthLimit,
		EnableDoubleClickEdit: setting.EnableDoubleClickEdit,
		Reactions:             setting.Reactions,
	}
}

func convertInstanceTagsSettingFromStore(setting *storepb.InstanceTagsSetting) *v1pb.InstanceSetting_TagsSetting {
	if setting == nil {
		return nil
	}
	tags := make(map[string]*v1pb.InstanceSetting_TagMetadata, len(setting.Tags))
	for tag, metadata := range setting.Tags {
		tags[tag] = &v1pb.InstanceSetting_TagMetadata{
			BackgroundColor: metadata.GetBackgroundColor(),
			BlurContent:     metadata.GetBlurContent(),
		}
	}
	return &v1pb.InstanceSetting_TagsSetting{
		Tags: tags,
	}
}

func convertInstanceTagsSettingToStore(setting *v1pb.InstanceSetting_TagsSetting) *storepb.InstanceTagsSetting {
	if setting == nil {
		return nil
	}
	tags := make(map[string]*storepb.InstanceTagMetadata, len(setting.Tags))
	for tag, metadata := range setting.Tags {
		tags[tag] = &storepb.InstanceTagMetadata{
			BackgroundColor: metadata.GetBackgroundColor(),
			BlurContent:     metadata.GetBlurContent(),
		}
	}
	return &storepb.InstanceTagsSetting{
		Tags: tags,
	}
}

func convertInstanceNotificationSettingFromStore(setting *storepb.InstanceNotificationSetting) *v1pb.InstanceSetting_NotificationSetting {
	if setting == nil {
		return nil
	}

	notificationSetting := &v1pb.InstanceSetting_NotificationSetting{}
	if setting.Email != nil {
		notificationSetting.Email = &v1pb.InstanceSetting_NotificationSetting_EmailSetting{
			Enabled:      setting.Email.Enabled,
			SmtpHost:     setting.Email.SmtpHost,
			SmtpPort:     setting.Email.SmtpPort,
			SmtpUsername: setting.Email.SmtpUsername,
			// SmtpPassword is write-only: never returned in responses.
			FromEmail: setting.Email.FromEmail,
			FromName:  setting.Email.FromName,
			ReplyTo:   setting.Email.ReplyTo,
			UseTls:    setting.Email.UseTls,
			UseSsl:    setting.Email.UseSsl,
		}
	}
	return notificationSetting
}

func convertInstanceNotificationSettingToStore(setting *v1pb.InstanceSetting_NotificationSetting) *storepb.InstanceNotificationSetting {
	if setting == nil {
		return nil
	}

	notificationSetting := &storepb.InstanceNotificationSetting{}
	if setting.Email != nil {
		notificationSetting.Email = &storepb.InstanceNotificationSetting_EmailSetting{
			Enabled:      setting.Email.Enabled,
			SmtpHost:     setting.Email.SmtpHost,
			SmtpPort:     setting.Email.SmtpPort,
			SmtpUsername: setting.Email.SmtpUsername,
			SmtpPassword: setting.Email.SmtpPassword,
			FromEmail:    setting.Email.FromEmail,
			FromName:     setting.Email.FromName,
			ReplyTo:      setting.Email.ReplyTo,
			UseTls:       setting.Email.UseTls,
			UseSsl:       setting.Email.UseSsl,
		}
	}
	return notificationSetting
}

func convertInstanceAISettingFromStore(setting *storepb.InstanceAISetting) *v1pb.InstanceSetting_AISetting {
	if setting == nil {
		return nil
	}

	aiSetting := &v1pb.InstanceSetting_AISetting{
		Providers:     make([]*v1pb.InstanceSetting_AIProviderConfig, 0, len(setting.Providers)),
		Transcription: convertTranscriptionConfigFromStore(setting.GetTranscription()),
	}
	for _, provider := range setting.Providers {
		if provider == nil {
			continue
		}
		apiKey := provider.GetApiKey()
		aiSetting.Providers = append(aiSetting.Providers, &v1pb.InstanceSetting_AIProviderConfig{
			Id:         provider.GetId(),
			Title:      provider.GetTitle(),
			Type:       v1pb.InstanceSetting_AIProviderType(provider.GetType()),
			Endpoint:   provider.GetEndpoint(),
			ApiKeySet:  apiKey != "",
			ApiKeyHint: maskAPIKey(apiKey),
		})
	}
	return aiSetting
}

func convertInstanceAISettingToStore(setting *v1pb.InstanceSetting_AISetting) *storepb.InstanceAISetting {
	if setting == nil {
		return nil
	}

	aiSetting := &storepb.InstanceAISetting{
		Providers:     make([]*storepb.AIProviderConfig, 0, len(setting.Providers)),
		Transcription: convertTranscriptionConfigToStore(setting.GetTranscription()),
	}
	for _, provider := range setting.Providers {
		if provider == nil {
			continue
		}
		aiSetting.Providers = append(aiSetting.Providers, &storepb.AIProviderConfig{
			Id:       provider.GetId(),
			Title:    provider.GetTitle(),
			Type:     storepb.AIProviderType(provider.GetType()),
			Endpoint: provider.GetEndpoint(),
			ApiKey:   provider.GetApiKey(),
		})
	}
	return aiSetting
}

func convertTranscriptionConfigFromStore(setting *storepb.TranscriptionConfig) *v1pb.InstanceSetting_TranscriptionConfig {
	if setting == nil {
		return nil
	}
	return &v1pb.InstanceSetting_TranscriptionConfig{
		ProviderId: setting.GetProviderId(),
		Model:      setting.GetModel(),
		Language:   setting.GetLanguage(),
		Prompt:     setting.GetPrompt(),
	}
}

func convertTranscriptionConfigToStore(setting *v1pb.InstanceSetting_TranscriptionConfig) *storepb.TranscriptionConfig {
	if setting == nil {
		return nil
	}
	return &storepb.TranscriptionConfig{
		ProviderId: setting.GetProviderId(),
		Model:      setting.GetModel(),
		Language:   setting.GetLanguage(),
		Prompt:     setting.GetPrompt(),
	}
}

func validateInstanceSetting(setting *v1pb.InstanceSetting) error {
	key, err := ExtractInstanceSettingKeyFromName(setting.Name)
	if err != nil {
		return err
	}
	if key != storepb.InstanceSettingKey_TAGS.String() {
		return nil
	}
	return validateInstanceTagsSetting(setting.GetTagsSetting())
}

func (s *APIV1Service) prepareInstanceAISettingForUpdate(ctx context.Context, setting *storepb.InstanceAISetting) error {
	if setting == nil {
		return errors.New("AI setting is required")
	}

	existing, err := s.Store.GetInstanceAISetting(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to get existing AI setting")
	}
	existingProviders := map[string]*storepb.AIProviderConfig{}
	if existing != nil {
		for _, provider := range existing.Providers {
			if provider != nil && provider.Id != "" {
				existingProviders[provider.Id] = provider
			}
		}
	}

	seenIDs := map[string]bool{}
	for _, provider := range setting.Providers {
		if provider == nil {
			return errors.New("provider cannot be nil")
		}

		provider.Id = strings.TrimSpace(provider.Id)
		if provider.Id == "" {
			provider.Id = shortuuid.New()
		}
		if seenIDs[provider.Id] {
			return errors.Errorf("duplicate provider ID %q", provider.Id)
		}
		seenIDs[provider.Id] = true

		provider.Title = strings.TrimSpace(provider.Title)
		if provider.Title == "" {
			return errors.New("provider title is required")
		}
		if provider.Type != storepb.AIProviderType_OPENAI && provider.Type != storepb.AIProviderType_GEMINI {
			return errors.Errorf("provider %q has unsupported type", provider.Id)
		}

		provider.Endpoint = strings.TrimSpace(provider.Endpoint)
		if provider.Type == storepb.AIProviderType_OPENAI && provider.Endpoint == "" {
			provider.Endpoint = "https://api.openai.com/v1"
		}
		if provider.Type == storepb.AIProviderType_GEMINI && provider.Endpoint == "" {
			provider.Endpoint = "https://generativelanguage.googleapis.com/v1beta"
		}

		if provider.ApiKey == "" {
			if existingProvider, ok := existingProviders[provider.Id]; ok {
				provider.ApiKey = existingProvider.ApiKey
			}
		}
		if provider.ApiKey == "" {
			return errors.Errorf("provider %q API key is required", provider.Id)
		}
	}

	if err := preparePersistedTranscriptionConfig(setting, existing); err != nil {
		return err
	}
	return nil
}

func preparePersistedTranscriptionConfig(setting *storepb.InstanceAISetting, existing *storepb.InstanceAISetting) error {
	// Preserve the previously stored transcription config when the request omits it,
	// matching the same "absence == keep" semantics used for API keys. The preserved
	// config still falls through to validation below, so a stale provider_id is
	// rejected if the same update removed or renamed its referenced provider.
	if setting.Transcription == nil && existing != nil {
		setting.Transcription = existing.GetTranscription()
	}
	if setting.Transcription == nil {
		return nil
	}

	cfg := setting.Transcription
	cfg.ProviderId = strings.TrimSpace(cfg.ProviderId)
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.Language = strings.TrimSpace(cfg.Language)
	cfg.Prompt = strings.TrimSpace(cfg.Prompt)

	if cfg.ProviderId != "" {
		referenced := false
		for _, provider := range setting.Providers {
			if provider != nil && provider.Id == cfg.ProviderId {
				referenced = true
				break
			}
		}
		if !referenced {
			return errors.Errorf("transcription provider_id %q does not reference any configured provider", cfg.ProviderId)
		}
	}

	if len(cfg.Model) > maxTranscriptionConfigModelLength {
		return errors.Errorf("transcription model is too long; maximum length is %d characters", maxTranscriptionConfigModelLength)
	}
	if len(cfg.Language) > maxTranscriptionConfigLanguageLength {
		return errors.Errorf("transcription language is too long; maximum length is %d characters", maxTranscriptionConfigLanguageLength)
	}
	if len(cfg.Prompt) > maxTranscriptionConfigPromptLength {
		return errors.Errorf("transcription prompt is too long; maximum length is %d characters", maxTranscriptionConfigPromptLength)
	}
	return nil
}

func maskAPIKey(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	if len(apiKey) <= 8 {
		return "..."
	}
	prefixLength := min(4, len(apiKey))
	return apiKey[:prefixLength] + "..." + apiKey[len(apiKey)-4:]
}

func validateInstanceTagsSetting(setting *v1pb.InstanceSetting_TagsSetting) error {
	if setting == nil {
		return errors.New("tags setting is required")
	}
	for tag, metadata := range setting.Tags {
		if strings.TrimSpace(tag) == "" {
			return errors.New("tag key cannot be empty")
		}
		if _, err := regexp.Compile(tag); err != nil {
			return errors.Errorf("tag key %q is not a valid regex pattern: %v", tag, err)
		}
		if metadata == nil {
			return errors.Errorf("tag metadata is required for %q", tag)
		}
		if metadata.GetBackgroundColor() != nil {
			if err := validateInstanceColor(metadata.GetBackgroundColor()); err != nil {
				return errors.Wrapf(err, "background_color for %q", tag)
			}
		}
	}
	return nil
}

func validateInstanceColor(color *colorpb.Color) error {
	if err := validateInstanceColorComponent("red", color.GetRed()); err != nil {
		return err
	}
	if err := validateInstanceColorComponent("green", color.GetGreen()); err != nil {
		return err
	}
	if err := validateInstanceColorComponent("blue", color.GetBlue()); err != nil {
		return err
	}
	if alpha := color.GetAlpha(); alpha != nil {
		if err := validateInstanceColorComponent("alpha", alpha.GetValue()); err != nil {
			return err
		}
	}
	return nil
}

func validateInstanceColorComponent(name string, value float32) error {
	if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
		return errors.Errorf("%s must be a finite number", name)
	}
	if value < 0 || value > 1 {
		return errors.Errorf("%s must be between 0 and 1", name)
	}
	return nil
}

func (s *APIV1Service) GetInstanceAdmin(ctx context.Context) (*v1pb.User, error) {
	adminUserType := store.RoleAdmin
	user, err := s.Store.GetUser(ctx, &store.FindUser{
		Role: &adminUserType,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to find admin")
	}
	if user == nil {
		return nil, nil
	}

	currentUser, _ := s.fetchCurrentUser(ctx)
	return convertUserFromStore(user, currentUser), nil
}
