package test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	v1pb "github.com/usememos/memos/proto/gen/api/v1"
	storepb "github.com/usememos/memos/proto/gen/store"
)

func TestTranscribe(t *testing.T) {
	ctx := context.Background()

	t.Run("requires authentication", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		_, err := ts.Service.Transcribe(ctx, &v1pb.TranscribeRequest{
			Audio: &v1pb.TranscriptionAudio{
				Source:      &v1pb.TranscriptionAudio_Content{Content: []byte("RIFF")},
				Filename:    "voice.wav",
				ContentType: "audio/wav",
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "user not authenticated")
	})

	t.Run("transcribes audio file using persisted transcription setting", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		user, err := ts.CreateRegularUser(ctx, "alice")
		require.NoError(t, err)
		userCtx := ts.CreateUserContext(ctx, user.ID)

		openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/audio/transcriptions", r.URL.Path)
			require.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
			require.NoError(t, r.ParseMultipartForm(10<<20))
			require.Equal(t, "whisper-1", r.FormValue("model"))
			require.Equal(t, "fr", r.FormValue("language"))
			require.Equal(t, "names: Alice", r.FormValue("prompt"))

			file, header, err := r.FormFile("file")
			require.NoError(t, err)
			defer file.Close()
			require.Equal(t, "voice.wav", header.Filename)

			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(map[string]string{
				"text": "transcribed text",
			}))
		}))
		defer openAIServer.Close()

		_, err = ts.Store.UpsertInstanceSetting(ctx, &storepb.InstanceSetting{
			Key: storepb.InstanceSettingKey_AI,
			Value: &storepb.InstanceSetting_AiSetting{
				AiSetting: &storepb.InstanceAISetting{
					Providers: []*storepb.AIProviderConfig{
						{
							Id:       "openai-main",
							Title:    "OpenAI",
							Type:     storepb.AIProviderType_OPENAI,
							Endpoint: openAIServer.URL,
							ApiKey:   "sk-test",
						},
					},
					Transcription: &storepb.TranscriptionConfig{
						ProviderId: "openai-main",
						Model:      "whisper-1",
						Language:   "fr",
						Prompt:     "names: Alice",
					},
				},
			},
		})
		require.NoError(t, err)

		resp, err := ts.Service.Transcribe(userCtx, &v1pb.TranscribeRequest{
			Audio: &v1pb.TranscriptionAudio{
				Source:      &v1pb.TranscriptionAudio_Content{Content: []byte("RIFF")},
				Filename:    "voice.wav",
				ContentType: "audio/wav",
			},
		})
		require.NoError(t, err)
		require.Equal(t, "transcribed text", resp.Text)
	})

	t.Run("returns provider error without rewriting it", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		user, err := ts.CreateRegularUser(ctx, "notfound-user")
		require.NoError(t, err)
		userCtx := ts.CreateUserContext(ctx, user.ID)

		openAIServer := httptest.NewServer(http.NotFoundHandler())
		defer openAIServer.Close()

		_, err = ts.Store.UpsertInstanceSetting(ctx, &storepb.InstanceSetting{
			Key: storepb.InstanceSettingKey_AI,
			Value: &storepb.InstanceSetting_AiSetting{
				AiSetting: &storepb.InstanceAISetting{
					Providers: []*storepb.AIProviderConfig{
						{
							Id:       "openai-main",
							Title:    "OpenAI",
							Type:     storepb.AIProviderType_OPENAI,
							Endpoint: openAIServer.URL,
							ApiKey:   "sk-test",
						},
					},
					Transcription: &storepb.TranscriptionConfig{
						ProviderId: "openai-main",
					},
				},
			},
		})
		require.NoError(t, err)

		_, err = ts.Service.Transcribe(userCtx, &v1pb.TranscribeRequest{
			Audio: &v1pb.TranscriptionAudio{
				Source:      &v1pb.TranscriptionAudio_Content{Content: []byte("RIFF")},
				Filename:    "voice.wav",
				ContentType: "audio/wav",
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "failed to transcribe audio")
	})

	t.Run("transcribes audio file with Gemini provider", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		user, err := ts.CreateRegularUser(ctx, "gemini-user")
		require.NoError(t, err)
		userCtx := ts.CreateUserContext(ctx, user.ID)

		geminiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/v1beta/models/gemini-2.5-flash:generateContent", r.URL.Path)
			require.Equal(t, "gemini-key", r.Header.Get("x-goog-api-key"))
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"candidates": []map[string]any{
					{
						"finishReason": "STOP",
						"content": map[string]any{
							"parts": []map[string]string{{"text": "gemini transcript"}},
						},
					},
				},
			}))
		}))
		defer geminiServer.Close()

		_, err = ts.Store.UpsertInstanceSetting(ctx, &storepb.InstanceSetting{
			Key: storepb.InstanceSettingKey_AI,
			Value: &storepb.InstanceSetting_AiSetting{
				AiSetting: &storepb.InstanceAISetting{
					Providers: []*storepb.AIProviderConfig{
						{
							Id:       "gemini-main",
							Title:    "Gemini",
							Type:     storepb.AIProviderType_GEMINI,
							Endpoint: geminiServer.URL + "/v1beta",
							ApiKey:   "gemini-key",
						},
					},
					Transcription: &storepb.TranscriptionConfig{
						ProviderId: "gemini-main",
					},
				},
			},
		})
		require.NoError(t, err)

		resp, err := ts.Service.Transcribe(userCtx, &v1pb.TranscribeRequest{
			Audio: &v1pb.TranscriptionAudio{
				Source:      &v1pb.TranscriptionAudio_Content{Content: []byte("mp3 bytes")},
				Filename:    "voice.mp3",
				ContentType: "audio/mp3",
			},
		})
		require.NoError(t, err)
		require.Equal(t, "gemini transcript", resp.Text)
	})

	t.Run("falls back to engine default model when transcription model is empty", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		user, err := ts.CreateRegularUser(ctx, "bob")
		require.NoError(t, err)
		userCtx := ts.CreateUserContext(ctx, user.ID)

		openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, r.ParseMultipartForm(10<<20))
			require.Equal(t, "whisper-1", r.FormValue("model"))
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(map[string]string{
				"text": "built-in model",
			}))
		}))
		defer openAIServer.Close()

		_, err = ts.Store.UpsertInstanceSetting(ctx, &storepb.InstanceSetting{
			Key: storepb.InstanceSettingKey_AI,
			Value: &storepb.InstanceSetting_AiSetting{
				AiSetting: &storepb.InstanceAISetting{
					Providers: []*storepb.AIProviderConfig{
						{
							Id:       "openai-main",
							Title:    "OpenAI",
							Type:     storepb.AIProviderType_OPENAI,
							Endpoint: openAIServer.URL,
							ApiKey:   "sk-test",
						},
					},
					Transcription: &storepb.TranscriptionConfig{
						ProviderId: "openai-main",
					},
				},
			},
		})
		require.NoError(t, err)

		resp, err := ts.Service.Transcribe(userCtx, &v1pb.TranscribeRequest{
			Audio: &v1pb.TranscriptionAudio{
				Source:      &v1pb.TranscriptionAudio_Content{Content: []byte("RIFF")},
				Filename:    "voice.wav",
				ContentType: "audio/wav",
			},
		})
		require.NoError(t, err)
		require.Equal(t, "built-in model", resp.Text)
	})

	t.Run("rejects non-audio content before provider call", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		user, err := ts.CreateRegularUser(ctx, "charlie")
		require.NoError(t, err)
		userCtx := ts.CreateUserContext(ctx, user.ID)

		_, err = ts.Service.Transcribe(userCtx, &v1pb.TranscribeRequest{
			Audio: &v1pb.TranscriptionAudio{
				Source:      &v1pb.TranscriptionAudio_Content{Content: []byte("not audio")},
				Filename:    "notes.txt",
				ContentType: "text/plain",
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "not supported")
	})

	t.Run("returns FailedPrecondition when transcription is not configured", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		user, err := ts.CreateRegularUser(ctx, "alice-empty")
		require.NoError(t, err)
		userCtx := ts.CreateUserContext(ctx, user.ID)

		_, err = ts.Service.Transcribe(userCtx, &v1pb.TranscribeRequest{
			Audio: &v1pb.TranscriptionAudio{
				Source:      &v1pb.TranscriptionAudio_Content{Content: []byte("RIFF")},
				Filename:    "voice.wav",
				ContentType: "audio/wav",
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "transcription is not configured")
	})

	t.Run("transcribes audio from attachment URI", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		user, err := ts.CreateRegularUser(ctx, "uri-user")
		require.NoError(t, err)
		userCtx := ts.CreateUserContext(ctx, user.ID)

		attachment, err := ts.Service.CreateAttachment(userCtx, &v1pb.CreateAttachmentRequest{
			Attachment: &v1pb.Attachment{
				Filename: "recording.wav",
				Type:     "audio/wav",
				Content:  []byte("RIFF audio data for URI test"),
			},
		})
		require.NoError(t, err)

		openAIServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, r.ParseMultipartForm(10<<20))
			require.Equal(t, "whisper-1", r.FormValue("model"))
			file, header, err := r.FormFile("file")
			require.NoError(t, err)
			defer file.Close()
			require.Equal(t, "recording.wav", header.Filename)

			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(map[string]string{
				"text": "uri transcribed text",
			}))
		}))
		defer openAIServer.Close()

		_, err = ts.Store.UpsertInstanceSetting(ctx, &storepb.InstanceSetting{
			Key: storepb.InstanceSettingKey_AI,
			Value: &storepb.InstanceSetting_AiSetting{
				AiSetting: &storepb.InstanceAISetting{
					Providers: []*storepb.AIProviderConfig{
						{
							Id:       "openai-main",
							Title:    "OpenAI",
							Type:     storepb.AIProviderType_OPENAI,
							Endpoint: openAIServer.URL,
							ApiKey:   "sk-test",
						},
					},
					Transcription: &storepb.TranscriptionConfig{
						ProviderId: "openai-main",
						Model:      "whisper-1",
					},
				},
			},
		})
		require.NoError(t, err)

		resp, err := ts.Service.Transcribe(userCtx, &v1pb.TranscribeRequest{
			Audio: &v1pb.TranscriptionAudio{
				Source: &v1pb.TranscriptionAudio_Uri{Uri: attachment.Name},
			},
		})
		require.NoError(t, err)
		require.Equal(t, "uri transcribed text", resp.Text)
	})

	t.Run("transcribes audio from attachment URI with Gemini provider", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		user, err := ts.CreateRegularUser(ctx, "uri-gemini-user")
		require.NoError(t, err)
		userCtx := ts.CreateUserContext(ctx, user.ID)

		attachment, err := ts.Service.CreateAttachment(userCtx, &v1pb.CreateAttachmentRequest{
			Attachment: &v1pb.Attachment{
				Filename: "recording.mp3",
				Type:     "audio/mp3",
				Content:  []byte("mp3 data for gemini URI test"),
			},
		})
		require.NoError(t, err)

		geminiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"candidates": []map[string]any{
					{
						"finishReason": "STOP",
						"content": map[string]any{
							"parts": []map[string]string{{"text": "gemini uri transcript"}},
						},
					},
				},
			}))
		}))
		defer geminiServer.Close()

		_, err = ts.Store.UpsertInstanceSetting(ctx, &storepb.InstanceSetting{
			Key: storepb.InstanceSettingKey_AI,
			Value: &storepb.InstanceSetting_AiSetting{
				AiSetting: &storepb.InstanceAISetting{
					Providers: []*storepb.AIProviderConfig{
						{
							Id:       "gemini-main",
							Title:    "Gemini",
							Type:     storepb.AIProviderType_GEMINI,
							Endpoint: geminiServer.URL + "/v1beta",
							ApiKey:   "gemini-key",
						},
					},
					Transcription: &storepb.TranscriptionConfig{
						ProviderId: "gemini-main",
					},
				},
			},
		})
		require.NoError(t, err)

		resp, err := ts.Service.Transcribe(userCtx, &v1pb.TranscribeRequest{
			Audio: &v1pb.TranscriptionAudio{
				Source: &v1pb.TranscriptionAudio_Uri{Uri: attachment.Name},
			},
		})
		require.NoError(t, err)
		require.Equal(t, "gemini uri transcript", resp.Text)
	})

	t.Run("rejects unknown attachment URI", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		user, err := ts.CreateRegularUser(ctx, "uri-notfound-user")
		require.NoError(t, err)
		userCtx := ts.CreateUserContext(ctx, user.ID)

		_, err = ts.Store.UpsertInstanceSetting(ctx, &storepb.InstanceSetting{
			Key: storepb.InstanceSettingKey_AI,
			Value: &storepb.InstanceSetting_AiSetting{
				AiSetting: &storepb.InstanceAISetting{
					Providers: []*storepb.AIProviderConfig{
						{
							Id:       "openai-main",
							Title:    "OpenAI",
							Type:     storepb.AIProviderType_OPENAI,
							Endpoint: "http://localhost:1",
							ApiKey:   "sk-test",
						},
					},
					Transcription: &storepb.TranscriptionConfig{
						ProviderId: "openai-main",
					},
				},
			},
		})
		require.NoError(t, err)

		_, err = ts.Service.Transcribe(userCtx, &v1pb.TranscribeRequest{
			Audio: &v1pb.TranscriptionAudio{
				Source: &v1pb.TranscriptionAudio_Uri{Uri: "attachments/nonexistent-uid"},
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "attachment not found")
	})

	t.Run("rejects attachment URI with unsupported file type", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		user, err := ts.CreateRegularUser(ctx, "uri-badtype-user")
		require.NoError(t, err)
		userCtx := ts.CreateUserContext(ctx, user.ID)

		attachment, err := ts.Service.CreateAttachment(userCtx, &v1pb.CreateAttachmentRequest{
			Attachment: &v1pb.Attachment{
				Filename: "notes.txt",
				Type:     "text/plain",
				Content:  []byte("not audio content"),
			},
		})
		require.NoError(t, err)

		_, err = ts.Store.UpsertInstanceSetting(ctx, &storepb.InstanceSetting{
			Key: storepb.InstanceSettingKey_AI,
			Value: &storepb.InstanceSetting_AiSetting{
				AiSetting: &storepb.InstanceAISetting{
					Providers: []*storepb.AIProviderConfig{
						{
							Id:       "openai-main",
							Title:    "OpenAI",
							Type:     storepb.AIProviderType_OPENAI,
							Endpoint: "http://localhost:1",
							ApiKey:   "sk-test",
						},
					},
					Transcription: &storepb.TranscriptionConfig{
						ProviderId: "openai-main",
					},
				},
			},
		})
		require.NoError(t, err)

		_, err = ts.Service.Transcribe(userCtx, &v1pb.TranscribeRequest{
			Audio: &v1pb.TranscriptionAudio{
				Source: &v1pb.TranscriptionAudio_Uri{Uri: attachment.Name},
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "not supported")
	})

	t.Run("rejects attachment URI when user lacks permission", func(t *testing.T) {
		ts := NewTestService(t)
		defer ts.Cleanup()

		owner, err := ts.CreateRegularUser(ctx, "attachment-owner")
		require.NoError(t, err)
		ownerCtx := ts.CreateUserContext(ctx, owner.ID)

		otherUser, err := ts.CreateRegularUser(ctx, "other-user")
		require.NoError(t, err)
		otherCtx := ts.CreateUserContext(ctx, otherUser.ID)

		attachment, err := ts.Service.CreateAttachment(ownerCtx, &v1pb.CreateAttachmentRequest{
			Attachment: &v1pb.Attachment{
				Filename: "private-recording.wav",
				Type:     "audio/wav",
				Content:  []byte("RIFF private audio"),
			},
		})
		require.NoError(t, err)

		_, err = ts.Store.UpsertInstanceSetting(ctx, &storepb.InstanceSetting{
			Key: storepb.InstanceSettingKey_AI,
			Value: &storepb.InstanceSetting_AiSetting{
				AiSetting: &storepb.InstanceAISetting{
					Providers: []*storepb.AIProviderConfig{
						{
							Id:       "openai-main",
							Title:    "OpenAI",
							Type:     storepb.AIProviderType_OPENAI,
							Endpoint: "http://localhost:1",
							ApiKey:   "sk-test",
						},
					},
					Transcription: &storepb.TranscriptionConfig{
						ProviderId: "openai-main",
					},
				},
			},
		})
		require.NoError(t, err)

		_, err = ts.Service.Transcribe(otherCtx, &v1pb.TranscribeRequest{
			Audio: &v1pb.TranscriptionAudio{
				Source: &v1pb.TranscriptionAudio_Uri{Uri: attachment.Name},
			},
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "permission denied")
	})
}
