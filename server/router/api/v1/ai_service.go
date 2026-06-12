package v1

import (
	"bytes"
	"context"
	"mime"
	"net/http"
	"strings"

	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/usememos/memos/internal/ai"
	"github.com/usememos/memos/internal/ai/audiollm"
	audiollmgemini "github.com/usememos/memos/internal/ai/audiollm/gemini"
	"github.com/usememos/memos/internal/ai/stt"
	sttopenai "github.com/usememos/memos/internal/ai/stt/openai"
	v1pb "github.com/usememos/memos/proto/gen/api/v1"
	storepb "github.com/usememos/memos/proto/gen/store"
	"github.com/usememos/memos/store"
)

const (
	maxTranscriptionAudioSizeBytes = 25 * MebiByte
	maxTranscriptionFilenameLength = 255
)

var supportedTranscriptionContentTypes = map[string]bool{
	"audio/aac":    true,
	"audio/aiff":   true,
	"audio/flac":   true,
	"audio/mpeg":   true,
	"audio/mp3":    true,
	"audio/mp4":    true,
	"audio/mpga":   true,
	"audio/ogg":    true,
	"audio/wav":    true,
	"audio/x-wav":  true,
	"audio/x-flac": true,
	"audio/x-m4a":  true,
	"audio/webm":   true,
	"video/mp4":    true,
	"video/mpeg":   true,
	"video/webm":   true,
}

// Transcribe transcribes an audio file using an instance AI provider.
// Audio can be supplied as inline bytes or by referencing an existing attachment URI.
func (s *APIV1Service) Transcribe(ctx context.Context, request *v1pb.TranscribeRequest) (*v1pb.TranscribeResponse, error) {
	user, err := s.fetchCurrentUser(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get current user: %v", err)
	}
	if user == nil {
		return nil, status.Errorf(codes.Unauthenticated, "user not authenticated")
	}

	if request.Audio == nil {
		return nil, status.Errorf(codes.InvalidArgument, "audio is required")
	}

	var content []byte
	var filename, contentType string

	switch {
	case request.Audio.GetUri() != "":
		content, filename, contentType, err = s.resolveTranscriptionAttachment(ctx, request.Audio)
		if err != nil {
			return nil, err
		}
	case len(request.Audio.GetContent()) > 0:
		content, filename, contentType, err = s.validateInlineTranscriptionAudio(request.Audio)
		if err != nil {
			return nil, err
		}
	default:
		return nil, status.Errorf(codes.InvalidArgument, "audio content or URI is required")
	}

	aiSetting, err := s.Store.GetInstanceAISetting(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get AI setting: %v", err)
	}
	persisted := aiSetting.GetTranscription()

	providerID := persisted.GetProviderId()
	if providerID == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "transcription is not configured")
	}

	provider, err := s.resolveAIProvider(aiSetting, providerID)
	if err != nil {
		return nil, err
	}

	model := persisted.GetModel()
	if model == "" {
		defaultModel, err := ai.DefaultTranscriptionModel(provider.Type)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%v", err)
		}
		model = defaultModel
	}

	var text string
	switch provider.Type {
	case ai.ProviderOpenAI:
		text, err = s.transcribeViaSTT(ctx, provider, persisted, model, content, filename, contentType)
	case ai.ProviderGemini:
		text, err = s.transcribeViaAudioLLM(ctx, provider, persisted, model, content, contentType)
	default:
		return nil, status.Errorf(codes.FailedPrecondition,
			"provider type %q is not supported for transcription", provider.Type)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to transcribe audio: %v", err)
	}
	return &v1pb.TranscribeResponse{Text: text}, nil
}

// resolveTranscriptionAttachment looks up an existing attachment by URI, checks
// permissions, validates file type and size, and returns the audio blob together
// with the filename and content type to use for transcription.
func (s *APIV1Service) resolveTranscriptionAttachment(ctx context.Context, audio *v1pb.TranscriptionAudio) ([]byte, string, string, error) {
	attachmentUID, err := ExtractAttachmentUIDFromName(audio.GetUri())
	if err != nil {
		return nil, "", "", status.Errorf(codes.InvalidArgument, "invalid attachment URI: %v", err)
	}

	attachment, err := s.Store.GetAttachment(ctx, &store.FindAttachment{UID: &attachmentUID, GetBlob: true})
	if err != nil {
		return nil, "", "", status.Errorf(codes.Internal, "failed to get attachment: %v", err)
	}
	if attachment == nil {
		return nil, "", "", status.Errorf(codes.NotFound, "attachment not found: %s", audio.GetUri())
	}

	if err := s.checkAttachmentAccess(ctx, attachment); err != nil {
		return nil, "", "", err
	}

	if !isSupportedTranscriptionContentType(attachment.Type) {
		return nil, "", "", status.Errorf(codes.InvalidArgument, "audio content type %q is not supported", attachment.Type)
	}
	if attachment.Size > maxTranscriptionAudioSizeBytes {
		return nil, "", "", status.Errorf(codes.InvalidArgument, "audio file is too large; maximum size is 25 MiB")
	}

	blob, err := s.GetAttachmentBlob(attachment)
	if err != nil {
		return nil, "", "", status.Errorf(codes.Internal, "failed to read attachment blob: %v", err)
	}

	filename := strings.TrimSpace(audio.GetFilename())
	if filename == "" {
		filename = attachment.Filename
	} else if len(filename) > maxTranscriptionFilenameLength {
		return nil, "", "", status.Errorf(codes.InvalidArgument, "filename is too long; maximum length is %d characters", maxTranscriptionFilenameLength)
	}

	return blob, filename, attachment.Type, nil
}

// validateInlineTranscriptionAudio validates inline audio bytes and returns the
// content, filename, and content type ready for provider dispatch.
func (*APIV1Service) validateInlineTranscriptionAudio(audio *v1pb.TranscriptionAudio) ([]byte, string, string, error) {
	content := audio.GetContent()
	if len(content) > maxTranscriptionAudioSizeBytes {
		return nil, "", "", status.Errorf(codes.InvalidArgument, "audio file is too large; maximum size is 25 MiB")
	}

	filename := strings.TrimSpace(audio.GetFilename())
	if len(filename) > maxTranscriptionFilenameLength {
		return nil, "", "", status.Errorf(codes.InvalidArgument, "filename is too long; maximum length is %d characters", maxTranscriptionFilenameLength)
	}

	contentType := strings.TrimSpace(audio.GetContentType())
	if contentType == "" {
		contentType = http.DetectContentType(content)
	}
	if !isSupportedTranscriptionContentType(contentType) {
		return nil, "", "", status.Errorf(codes.InvalidArgument, "audio content type %q is not supported", contentType)
	}

	return content, filename, contentType, nil
}

func (*APIV1Service) transcribeViaSTT(
	ctx context.Context,
	provider ai.ProviderConfig,
	persisted *storepb.TranscriptionConfig,
	model string,
	content []byte,
	filename string,
	contentType string,
) (string, error) {
	transcriber, err := sttopenai.New(provider, stt.ApplyOptions(nil))
	if err != nil {
		return "", errors.Wrap(err, "failed to create STT transcriber")
	}
	resp, err := transcriber.Transcribe(ctx, stt.Request{
		Audio:       bytes.NewReader(content),
		Size:        int64(len(content)),
		Filename:    filename,
		ContentType: contentType,
		Model:       model,
		Prompt:      persisted.GetPrompt(),
		Language:    persisted.GetLanguage(),
	})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}

func (*APIV1Service) transcribeViaAudioLLM(
	ctx context.Context,
	provider ai.ProviderConfig,
	persisted *storepb.TranscriptionConfig,
	model string,
	content []byte,
	contentType string,
) (string, error) {
	m, err := audiollmgemini.New(provider, audiollm.ApplyOptions(nil))
	if err != nil {
		return "", errors.Wrap(err, "failed to create audio LLM")
	}
	resp, err := m.GenerateFromAudio(ctx, audiollm.Request{
		Audio:        bytes.NewReader(content),
		Size:         int64(len(content)),
		ContentType:  contentType,
		Model:        model,
		Instructions: buildTranscriptionInstructions(persisted.GetPrompt(), persisted.GetLanguage()),
	})
	if err != nil {
		return "", err
	}
	if resp.FinishReason != audiollm.FinishStop {
		return "", errors.Errorf("transcription incomplete (finish reason: %s)", resp.FinishReason)
	}
	if strings.TrimSpace(resp.Text) == "" {
		return "", errors.New("transcription response did not include text")
	}
	return resp.Text, nil
}

func buildTranscriptionInstructions(prompt, language string) string {
	parts := []string{
		"Transcribe the audio accurately. Return only the transcript text. " +
			"Do not summarize, explain, or add content that is not spoken.",
	}
	if language = strings.TrimSpace(language); language != "" {
		parts = append(parts, "The input language is "+language+".")
	}
	if prompt = strings.TrimSpace(prompt); prompt != "" {
		parts = append(parts, "Context and spelling hints:\n"+prompt)
	}
	return strings.Join(parts, "\n\n")
}

func (*APIV1Service) resolveAIProvider(setting *storepb.InstanceAISetting, providerID string) (ai.ProviderConfig, error) {
	providers := make([]ai.ProviderConfig, 0, len(setting.GetProviders()))
	for _, provider := range setting.GetProviders() {
		if provider == nil {
			continue
		}
		providers = append(providers, convertAIProviderConfigFromStore(provider))
	}

	provider, err := ai.FindProvider(providers, providerID)
	if err != nil {
		return ai.ProviderConfig{}, status.Errorf(codes.FailedPrecondition, "transcription provider is not configured")
	}
	return *provider, nil
}

func convertAIProviderConfigFromStore(provider *storepb.AIProviderConfig) ai.ProviderConfig {
	return ai.ProviderConfig{
		ID:       provider.GetId(),
		Title:    provider.GetTitle(),
		Type:     convertAIProviderTypeFromStore(provider.GetType()),
		Endpoint: provider.GetEndpoint(),
		APIKey:   provider.GetApiKey(),
	}
}

func convertAIProviderTypeFromStore(providerType storepb.AIProviderType) ai.ProviderType {
	switch providerType {
	case storepb.AIProviderType_OPENAI:
		return ai.ProviderOpenAI
	case storepb.AIProviderType_GEMINI:
		return ai.ProviderGemini
	default:
		return ""
	}
}

func isSupportedTranscriptionContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	return supportedTranscriptionContentTypes[mediaType]
}
