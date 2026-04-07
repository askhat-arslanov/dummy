package ai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"code.gitea.io/gitea/modules/log"
	apiError "code.gitea.io/gitea/routers/sbt/apierror"
	"code.gitea.io/gitea/routers/sbt/request"
	"code.gitea.io/gitea/routers/sbt/response"
	"code.gitea.io/gitea/services/ai"
	"code.gitea.io/gitea/services/context"
)

func SolveTask(ctx *context.Context) {
	log.WithCtx(ctx).Infof("[AI] ===== SolveTask Called =====")
	log.WithCtx(ctx).Infof("[AI] Method: %s %s", ctx.Req.Method, ctx.Req.URL)

	// чекаем авторизацию
	if ctx.Doer == nil || !ctx.Doer.IsActive {
		log.WithCtx(ctx).Errorf("[AI] User not authenticated")
		ctx.JSON(http.StatusUnauthorized, response.AITaskResponse{
			Status: "error",
			Error:  "user not authenticated",
		})
		return
	}

	// читаем body ОДИН раз
	bodyBytes, err := io.ReadAll(ctx.Req.Body)
	if err != nil {
		log.WithCtx(ctx).Errorf("[AI] Failed to read body: %v", err)
		ctx.JSON(http.StatusBadRequest, response.AITaskResponse{
			Status: "error",
			Error:  fmt.Sprintf("failed to read body: %v", err),
		})
		return
	}

	log.WithCtx(ctx).Infof("[AI] Body length: %d bytes", len(bodyBytes))

	if len(bodyBytes) == 0 {
		log.WithCtx(ctx).Errorf("[AI] Body is empty")
		ctx.JSON(http.StatusBadRequest, response.AITaskResponse{
			Status: "error",
			Error:  "body is empty",
		})
		return
	}

	var req request.AITaskRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		log.WithCtx(ctx).Errorf("[AI] Failed to parse JSON: %v", err)
		ctx.JSON(http.StatusBadRequest, response.AITaskResponse{
			Status: "error",
			Error:  fmt.Sprintf("invalid json: %v", err),
		})
		return
	}

	log.WithCtx(ctx).Infof("[AI] Parsed task: %s", req.Task)
	log.WithCtx(ctx).Infof("[AI] Session ID from request: %s", req.SessionID)

	if req.Metadata != nil {
		log.WithCtx(ctx).Infof("[AI] Metadata received: page_type=%s, owner=%s, repo=%s, pr=%d, branch=%s, file=%s, run=%s, workflow=%s, user=%s", // FIXME: убрать логи
			req.Metadata.PageType,
			req.Metadata.OwnerName,
			req.Metadata.RepositoryName,
			req.Metadata.PullRequestID,
			req.Metadata.BranchOrTag,
			req.Metadata.FilePath,
			req.Metadata.RunID,
			req.Metadata.WorkflowName,
			req.Metadata.UserName)
	} else {
		log.WithCtx(ctx).Infof("[AI] No metadata in request")
	}

	if req.Task == "" {
		log.WithCtx(ctx).Errorf("[AI] Task is empty")
		ctx.JSON(http.StatusBadRequest, response.AITaskResponse{
			Status: "error",
			Error:  "task is required",
		})
		return
	}

	log.WithCtx(ctx).Infof("[AI] Solving task: %s for user %d", req.Task, ctx.Doer.ID)

	sessionID := req.SessionID
	isNewSession := false

	if sessionID == "" {
		isNewSession = true
		log.WithCtx(ctx).Infof("[AI] New chat requested (no sessionId from client)")
	} else {
		log.WithCtx(ctx).Infof("[AI] Using session from request: %s", sessionID)
	}

	// FIXME: блокирует запрос на 120 сек
	result, returnedSessionID, err := callPluginAPI(ctx, req.Task, sessionID, req.Metadata, isNewSession)
	if err != nil {
		log.WithCtx(ctx).Errorf("[AI] Error calling plugin: %v", err)
		ctx.JSON(http.StatusInternalServerError, response.AITaskResponse{
			Status:    "error",
			Error:     err.Error(),
			SessionID: sessionID,
		})
		return nil
	}

	if isNewSession {
		log.WithCtx(ctx).Infof("[AI] First response - plugin returned session: %s", returnedSessionID)
	} else if returnedSessionID != sessionID {
		log.WithCtx(ctx).Infof("[AI] Session updated from plugin: %s → %s", sessionID, returnedSessionID)
	}

	log.WithCtx(ctx).Infof("[AI] Task completed successfully. Session: %s", returnedSessionID)

	ctx.JSON(http.StatusOK, response.AITaskResponse{
		Status:       result.Status,
		Result:       result.Result,
		SessionID:    returnedSessionID,
		MessageId:    result.MessageId,
		MessageType:  result.MessageType,
		Confirmation: result.Confirmation,
	})
}

func callPluginAPI(ctx *context.Context, task string, sessionID string, metadata *request.AIMetadata, isNewSession bool) (response.AITaskResponse, string, error) {
	requestBody := request.AITaskRequest{
		Task:      task,
		SessionID: sessionID,
		Metadata:  metadata,
	}

	resp, status, err := ai.CallAIChatAgentAPI(ctx, "POST", "/message/send", requestBody)
	if err != nil {
		log.WithCtx(ctx).Errorf("[AI] Error calling plugin: %v", err)
		ctx.JSON(status, apiError.ApiError{Code: 1000, Message: err.Error()})
		return response.AITaskResponse{}, "", fmt.Errorf("failed to call plugin: %w", err)
	}
	defer resp.Close()

	var pluginResponse response.AITaskResponse
	if err = json.NewDecoder(resp).Decode(&pluginResponse); err != nil {
		return response.AITaskResponse{}, "", fmt.Errorf("failed to decode plugin response: %w", err)
	}

	if pluginResponse.Status != "success" {
		return pluginResponse, "", fmt.Errorf("plugin returned error: %s", pluginResponse.Error)
	}

	returnedSessionID := pluginResponse.SessionID
	if returnedSessionID == "" {
		returnedSessionID = sessionID
		log.WithCtx(ctx).Warnf("[AI] Plugin did not return session_id, using: %s", sessionID)
	}

	if isNewSession && returnedSessionID != sessionID {
		log.WithCtx(ctx).Infof("[AI] First request: temporary session %s → plugin session %s", sessionID, returnedSessionID)
	}

	log.WithCtx(ctx).Infof("[AI] Plugin response: status=%s, result_len=%d, session=%s",
		pluginResponse.Status, len(pluginResponse.Result), returnedSessionID)

	return pluginResponse, returnedSessionID, nil
}
