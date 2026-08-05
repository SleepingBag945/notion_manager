package proxy

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestWorkflowRequestProtocolJSON(t *testing.T) {
	reqBody := NotionInferenceRequest{
		Transcript: []interface{}{},
		ThreadType: "workflow",
	}
	applyWorkflowRequestProtocol(&reqBody, "ai_module")

	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	for key, want := range map[string]interface{}{
		"setUnreadState":                         true,
		"createdSource":                          "ai_module",
		"asPatchResponse":                        true,
		"patchResponseVersion":                   float64(2),
		"isUserInAnySalesAssistedSpace":          false,
		"isSpaceSalesAssisted":                   false,
		"supportsCustomAgentNudgeTranscriptStep": true,
	} {
		if value, ok := got[key]; !ok || !reflect.DeepEqual(value, want) {
			t.Fatalf("serialized %s = %#v (present=%v), want %#v", key, value, ok, want)
		}
	}

	wantDebugOverrides := map[string]interface{}{
		"emitAgentSearchExtractedResults": true,
		"cachedInferences":                map[string]interface{}{},
		"annotationInferences":            map[string]interface{}{},
		"emitInferences":                  false,
	}
	if gotDebugOverrides, ok := got["debugOverrides"].(map[string]interface{}); !ok || !reflect.DeepEqual(gotDebugOverrides, wantDebugOverrides) {
		t.Fatalf("serialized debugOverrides = %#v, want %#v", got["debugOverrides"], wantDebugOverrides)
	}
}

func TestResearcherRequestProtocolJSONRemainsUnchanged(t *testing.T) {
	reqBody := NotionInferenceRequest{
		Transcript: []interface{}{},
		ThreadType: "researcher",
		DebugOverrides: DebugOverrides{
			EmitAgentSearchExtractedResults: true,
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	for _, key := range []string{
		"createdSource",
		"patchResponseVersion",
		"isUserInAnySalesAssistedSpace",
		"isSpaceSalesAssisted",
		"supportsCustomAgentNudgeTranscriptStep",
	} {
		if _, ok := got[key]; ok {
			t.Fatalf("researcher request unexpectedly serialized %s", key)
		}
	}

	wantDebugOverrides := map[string]interface{}{
		"emitAgentSearchExtractedResults": true,
	}
	if gotDebugOverrides, ok := got["debugOverrides"].(map[string]interface{}); !ok || !reflect.DeepEqual(gotDebugOverrides, wantDebugOverrides) {
		t.Fatalf("serialized researcher debugOverrides = %#v, want %#v", got["debugOverrides"], wantDebugOverrides)
	}
}

func TestBuildConfigValueMatchesModernWorkflowProtocol(t *testing.T) {
	previous := AppConfig
	AppConfig = DefaultConfig()
	defer func() { AppConfig = previous }()

	workspaceSearch := false
	for _, isSubsequentTurn := range []bool{false, true} {
		got := buildConfigValue("acai-budino-high", "", false, true, &workspaceSearch, false, false, isSubsequentTurn)
		want := expectedModernWorkflowConfig(isSubsequentTurn)
		if !reflect.DeepEqual(got, want) {
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			t.Fatalf("buildConfigValue(subsequent=%v) = %s, want %s", isSubsequentTurn, gotJSON, wantJSON)
		}
	}
}

func TestBuildConfigValueCarriesCapturedReasoningEffortForAllTurns(t *testing.T) {
	previous := AppConfig
	AppConfig = DefaultConfig()
	defer func() { AppConfig = previous }()

	for _, isSubsequentTurn := range []bool{false, true} {
		got := buildConfigValue("strawberry-whoopiepie", "high", false, true, nil, false, false, isSubsequentTurn)
		if got["reasoningEffort"] != "high" {
			t.Fatalf("reasoningEffort(subsequent=%v) = %#v, want high", isSubsequentTurn, got["reasoningEffort"])
		}
	}

	withoutEffort := buildConfigValue("strawberry-whoopiepie", "", false, true, nil, false, false, false)
	if _, ok := withoutEffort["reasoningEffort"]; ok {
		t.Fatalf("reasoningEffort unexpectedly present when the client omitted it")
	}
}

func TestNormalizeReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "empty", value: "", want: ""},
		{name: "high normalized", value: " HIGH ", want: "high"},
		{name: "native maximum", value: "max", want: "max"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeReasoningEffort(tc.value)
			if err != nil {
				t.Fatalf("normalizeReasoningEffort(%q) error = %v", tc.value, err)
			}
			if got != tc.want {
				t.Fatalf("normalizeReasoningEffort(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}

	if _, err := normalizeReasoningEffort("auto"); err == nil {
		t.Fatal("normalizeReasoningEffort(auto) unexpectedly succeeded")
	}
}

func TestBuildWebSearchCallOptionsCarriesReasoningEffort(t *testing.T) {
	got := buildWebSearchCallOptions("request-id", "high")
	if !got.EnableWebSearch || got.ReasoningEffort != "high" || got.RequestID != "request-id" {
		t.Fatalf("buildWebSearchCallOptions() = %#v", got)
	}
}

func TestTranscriptJSONCarriesCapturedReasoningEffortForAllTurns(t *testing.T) {
	previous := AppConfig
	AppConfig = DefaultConfig()
	defer func() { AppConfig = previous }()

	account := protocolTestAccount()
	full := buildFullTranscript(
		account,
		[]ChatMessage{{Role: "user", Content: "hello"}},
		"strawberry-whoopiepie",
		"high",
		false,
		true,
		nil,
		false,
		nil,
		"config-id",
		"context-id",
		"context-page-id",
		"2026-08-05T01:02:03Z",
	)
	partial := buildPartialTranscript(account, "continue", "strawberry-whoopiepie", "high", false, true, nil, false, nil, &Session{
		ConfigID:         "config-id",
		ContextID:        "context-id",
		ContextPageID:    "context-page-id",
		OriginalDatetime: "2026-08-05T01:02:03Z",
	})

	for name, transcript := range map[string][]interface{}{"full": full, "partial": partial} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(NotionInferenceRequest{Transcript: transcript, ThreadType: "workflow"})
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			var got struct {
				Transcript []struct {
					Type  string          `json:"type"`
					Value json.RawMessage `json:"value"`
				} `json:"transcript"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}
			if len(got.Transcript) == 0 || got.Transcript[0].Type != "config" {
				t.Fatalf("transcript[0] = %#v, want config", got.Transcript)
			}
			var config map[string]interface{}
			if err := json.Unmarshal(got.Transcript[0].Value, &config); err != nil {
				t.Fatalf("json.Unmarshal(transcript[0].value) error = %v", err)
			}
			if config["model"] != "strawberry-whoopiepie" || config["reasoningEffort"] != "high" || config["modelFromUser"] != true {
				t.Fatalf("transcript[0].value = %#v", config)
			}
		})
	}
}

func TestBuildConfigValuePreservesDynamicToolFlags(t *testing.T) {
	previous := AppConfig
	AppConfig = DefaultConfig()
	defer func() { AppConfig = previous }()

	workspaceSearch := false
	got := buildConfigValue("acai-budino-high", "", true, false, &workspaceSearch, true, false, false)
	for _, key := range []string{
		"enableAgentAutomations",
		"enableAgentIntegrations",
		"enableCustomAgents",
		"enableAgentDiffs",
		"enableScriptAgent",
		"enableScriptAgentSlack",
		"enableScriptAgentMcpServers",
		"enableComputer",
		"enableAgentGenerateImage",
	} {
		if value, ok := got[key].(bool); !ok || value {
			t.Fatalf("%s = %#v, want false when built-in tools are disabled", key, got[key])
		}
	}
	if got["useWebSearch"] != false {
		t.Fatalf("useWebSearch = %#v, want false", got["useWebSearch"])
	}
	if got["useReadOnlyMode"] != true {
		t.Fatalf("useReadOnlyMode = %#v, want true", got["useReadOnlyMode"])
	}
	if _, ok := got["searchScopes"]; ok {
		t.Fatalf("searchScopes unexpectedly present when web and workspace search are disabled")
	}
}

func TestBuildFullTranscriptUsesIndependentContextPageID(t *testing.T) {
	previous := AppConfig
	AppConfig = DefaultConfig()
	defer func() { AppConfig = previous }()

	account := protocolTestAccount()
	transcript := buildFullTranscript(
		account,
		[]ChatMessage{{Role: "user", Content: "hello"}},
		"acai-budino-high",
		"",
		false,
		true,
		nil,
		false,
		nil,
		"config-id",
		"context-id",
		"context-page-id",
		"2026-08-03T01:02:03Z",
	)
	if len(transcript) != 3 {
		t.Fatalf("transcript length = %d, want 3", len(transcript))
	}

	context := requireResearcherTranscriptMsg(t, transcript[1], "context")
	if context.ID != "context-id" {
		t.Fatalf("context id = %q, want context-id", context.ID)
	}
	contextValue := requireTranscriptValueMap(t, context)
	for key, want := range map[string]interface{}{
		"surface":         "ai_module",
		"agentAccessory":  "paprika",
		"context_page_id": "context-page-id",
		"currentDatetime": "2026-08-03T01:02:03Z",
	} {
		if got := contextValue[key]; got != want {
			t.Fatalf("context %s = %#v, want %#v", key, got, want)
		}
	}
	if context.ID == contextValue["context_page_id"] {
		t.Fatalf("context transcript id must differ from context_page_id")
	}

	user := requireResearcherTranscriptMsg(t, transcript[2], "user")
	if user.CreatedAt != "2026-08-03T01:02:03Z" {
		t.Fatalf("user createdAt = %q, want first-turn context datetime", user.CreatedAt)
	}
}

func TestWorkflowRequestProtocolAttachmentSourceMatchesContext(t *testing.T) {
	previous := AppConfig
	AppConfig = DefaultConfig()
	defer func() { AppConfig = previous }()

	transcript := buildFullTranscript(
		protocolTestAccount(),
		[]ChatMessage{{Role: "user", Content: "summarize"}},
		"acai-budino-high",
		"",
		false,
		true,
		nil,
		false,
		[]UploadedAttachment{{AttachmentURL: "attachment:redacted", FileName: "redacted.txt", ContentType: "text/plain"}},
		"config-id",
		"context-id",
		"context-page-id",
		"2026-08-03T01:02:03Z",
	)
	if len(transcript) != 4 {
		t.Fatalf("transcript length = %d, want config + context + attachment + user", len(transcript))
	}
	context := requireResearcherTranscriptMsg(t, transcript[1], "context")
	contextSource, ok := requireTranscriptValueMap(t, context)["surface"].(string)
	if !ok || contextSource != "workflows" {
		t.Fatalf("attachment context source = %#v, want workflows", contextSource)
	}
	attachment, ok := transcript[2].(AttachmentTranscriptMsg)
	if !ok || attachment.ID == "" || attachment.Metadata.Guardrail == nil || attachment.Metadata.Guardrail.AttachmentRisk != "skipped" {
		t.Fatalf("attachment transcript = %#v, want native attachment fields", transcript[2])
	}
	user := requireResearcherTranscriptMsg(t, transcript[3], "user")
	if user.Value == nil {
		t.Fatal("user transcript unexpectedly empty after attachment")
	}

	reqBody := NotionInferenceRequest{Transcript: transcript, ThreadType: "workflow"}
	applyWorkflowRequestProtocol(&reqBody, contextSource)
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got["createdSource"] != contextSource {
		t.Fatalf("attachment createdSource = %#v, want context source %q", got["createdSource"], contextSource)
	}
}

func TestBuildPartialTranscriptUsesTwoContextsAndReusesContextPageID(t *testing.T) {
	previous := AppConfig
	AppConfig = DefaultConfig()
	defer func() { AppConfig = previous }()

	account := protocolTestAccount()
	session := &Session{
		ConfigID:         "config-id",
		ContextID:        "context-id",
		OriginalDatetime: "2026-08-03T01:02:03Z",
		UpdatedConfigIDs: []string{"updated-one", "updated-two"},
	}

	transcript := buildPartialTranscript(account, "next question", "acai-budino-high", "", false, true, nil, false, nil, session)
	if len(transcript) != 6 {
		t.Fatalf("transcript length = %d, want 6", len(transcript))
	}
	if session.ContextPageID == "" || session.ContextPageID == session.ContextID || session.ContextPageID == session.ConfigID {
		t.Fatalf("generated ContextPageID = %q, want an independent UUID", session.ContextPageID)
	}

	originalContext := requireResearcherTranscriptMsg(t, transcript[1], "context")
	if originalContext.ID != session.ContextID {
		t.Fatalf("original context id = %q, want %q", originalContext.ID, session.ContextID)
	}
	originalContextValue := requireTranscriptValueMap(t, originalContext)
	if originalContextValue["surface"] != "ai_module" {
		t.Fatalf("original context surface = %#v, want ai_module", originalContextValue["surface"])
	}
	if originalContextValue["currentDatetime"] != session.OriginalDatetime {
		t.Fatalf("original context currentDatetime = %#v, want %q", originalContextValue["currentDatetime"], session.OriginalDatetime)
	}

	currentContext := requireResearcherTranscriptMsg(t, transcript[2], "context")
	currentContextValue := requireTranscriptValueMap(t, currentContext)
	if currentContext.ID == session.ContextID || currentContext.ID == session.ContextPageID {
		t.Fatalf("current context id = %q, want a new transcript UUID", currentContext.ID)
	}
	if currentContextValue["surface"] != "full_page_chat" {
		t.Fatalf("current context surface = %#v, want full_page_chat", currentContextValue["surface"])
	}
	for _, value := range []map[string]interface{}{originalContextValue, currentContextValue} {
		if value["context_page_id"] != session.ContextPageID {
			t.Fatalf("context_page_id = %#v, want reused %q", value["context_page_id"], session.ContextPageID)
		}
		if value["agentAccessory"] != "paprika" {
			t.Fatalf("agentAccessory = %#v, want paprika", value["agentAccessory"])
		}
	}

	for index, wantID := range []string{"updated-one", "updated-two"} {
		updated, ok := transcript[index+3].(UpdatedConfigMsg)
		if !ok || updated.ID != wantID || updated.Type != "updated-config" {
			t.Fatalf("updated config %d = %#v, want id=%q type=updated-config", index, transcript[index+3], wantID)
		}
	}

	user := requireResearcherTranscriptMsg(t, transcript[5], "user")
	if user.CreatedAt != currentContextValue["currentDatetime"] {
		t.Fatalf("user createdAt = %q, current context datetime = %#v", user.CreatedAt, currentContextValue["currentDatetime"])
	}

	contextPageID := session.ContextPageID
	buildPartialTranscript(account, "another question", "acai-budino-high", "", false, true, nil, false, nil, session)
	if session.ContextPageID != contextPageID {
		t.Fatalf("ContextPageID changed from %q to %q", contextPageID, session.ContextPageID)
	}
}

func TestParseNDJSONStreamHandlesCapturedPatchV2Sequence(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"patch-start","version":1,"data":{"s":[{}]}}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"step-1","type":"config","value":{}}}]}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"step-2","type":"context","value":{}}}]}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"step-3","type":"context","value":{}}}]}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"step-4","type":"updated-config"}}]}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"step-5","type":"user","value":[["redacted"]]}}]}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"step-6","type":"assistant-reply","value":[]}}]}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"step-redacted","type":"agent-inference","value":[{"type":"text","content":"O"}],"traceId":"trace-redacted","startedAt":1,"previousAttemptValues":[]}}]}`,
		`{"type":"patch","v":[{"o":"x","p":"/s/7/value/0/content","v":"K"}]}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/7/inputTokens","v":12},{"o":"a","p":"/s/7/outputTokens","v":2}]}`,
		`{"type":"record-map","recordMap":{"block":{}}}`,
		`{"type":"patch-sync","version":1}`,
	}, "\n")

	var deltas []string
	var finalUsage UsageInfo
	doneCount := 0
	err := parseNDJSONStream(bytes.NewBufferString(stream), "", func(delta string, done bool, usage *UsageInfo) {
		if delta != "" {
			deltas = append(deltas, delta)
		}
		if done {
			doneCount++
			if usage != nil {
				finalUsage = *usage
			}
		}
	}, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("parseNDJSONStream() error = %v", err)
	}

	if !reflect.DeepEqual(deltas, []string{"O", "K"}) {
		t.Fatalf("text deltas = %#v, want [O K]", deltas)
	}
	if strings.Join(deltas, "") != "OK" {
		t.Fatalf("joined text = %q, want OK", strings.Join(deltas, ""))
	}
	if doneCount != 1 {
		t.Fatalf("done callbacks = %d, want 1", doneCount)
	}
	wantUsage := UsageInfo{PromptTokens: 12, CompletionTokens: 2, TotalTokens: 14}
	if finalUsage != wantUsage {
		t.Fatalf("usage = %+v, want %+v", finalUsage, wantUsage)
	}
}

func TestParseNDJSONStreamPatchV2ForwardsToolUseEntries(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"patch-start","version":1,"data":{"s":[{}]}}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"step-redacted","type":"agent-inference","value":[{"type":"text","content":"A"},{"type":"tool_use","id":"tool-initial","name":"initialTool","content":"DO_NOT_EMIT","input":{"value":1}}]}}]}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/1/value/-","v":{"type":"text","content":"B"}}]}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/1/value/-","v":{"type":"tool_use","id":"tool-later","name":"laterTool","content":"DO_NOT_EMIT","input":{"value":2}}}]}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/1/value/-","v":{"type":"tool_use","id":"tool-later","name":"laterTool","content":"DO_NOT_EMIT","input":{"value":2}}}]}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/1/value/-","v":{"type":"tool_use","id":"tool-initial","name":"initialTool","content":"DO_NOT_EMIT","input":{"value":1}}}]}`,
	}, "\n")

	var textOutput strings.Builder
	var nativeToolUses []AgentValueEntry
	err := parseNDJSONStream(bytes.NewBufferString(stream), "", func(delta string, done bool, usage *UsageInfo) {
		textOutput.WriteString(delta)
	}, &nativeToolUses, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("parseNDJSONStream() error = %v", err)
	}

	if textOutput.String() != "AB" {
		t.Fatalf("text output = %q, want AB without tool content", textOutput.String())
	}
	if len(nativeToolUses) != 2 {
		t.Fatalf("native tool uses = %#v, want two deduplicated entries", nativeToolUses)
	}
	if nativeToolUses[0].ID != "tool-initial" || nativeToolUses[0].Name != "initialTool" {
		t.Fatalf("initial native tool use = %#v", nativeToolUses[0])
	}
	if nativeToolUses[1].ID != "tool-later" || nativeToolUses[1].Name != "laterTool" {
		t.Fatalf("later native tool use = %#v", nativeToolUses[1])
	}
}

func TestParseNDJSONStreamPatchV2FlushesStepThinking(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"patch-start","version":1,"data":{"s":[{}]}}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/-","v":{"id":"step-redacted","type":"agent-inference","value":[{"type":"thinking","content":"Plan"}]}}]}`,
		`{"type":"patch","v":[{"o":"x","p":"/s/1/value/0/content","v":" more"}]}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/1/value/0/signature","v":"signature-redacted"}]}`,
		`{"type":"patch","v":[{"o":"a","p":"/s/1/finishedAt","v":2}]}`,
	}, "\n")

	var thinkingOutput strings.Builder
	var thinkingBlocks []ThinkingBlock
	err := parseNDJSONStream(bytes.NewBufferString(stream), "", func(delta string, done bool, usage *UsageInfo) {}, nil, &thinkingBlocks, func(delta string, done bool, signature string) {
		thinkingOutput.WriteString(delta)
	}, nil, nil, nil)
	if err != nil {
		t.Fatalf("parseNDJSONStream() error = %v", err)
	}

	if thinkingOutput.String() != "Plan more" {
		t.Fatalf("thinking output = %q, want Plan more", thinkingOutput.String())
	}
	wantBlocks := []ThinkingBlock{{Content: "Plan more", Signature: "signature-redacted"}}
	if !reflect.DeepEqual(thinkingBlocks, wantBlocks) {
		t.Fatalf("thinking blocks = %#v, want %#v", thinkingBlocks, wantBlocks)
	}
}

func expectedModernWorkflowConfig(isSubsequentTurn bool) map[string]interface{} {
	want := map[string]interface{}{
		"type":                                           "workflow",
		"enableAgentAutomations":                         true,
		"enableAgentIntegrations":                        true,
		"enableCustomAgents":                             true,
		"enableExperimentalIntegrations":                 false,
		"enableScriptAgent":                              true,
		"enableScriptAgentAdvanced":                      false,
		"enableScriptAgentSearchConnectorsInCustomAgent": false,
		"enableScriptAgentGoogleDriveInCustomAgent":      false,
		"enableScriptAgentGoogleDriveOAuthInCustomAgent": false,
		"enableScriptAgentSlack":                         true,
		"enableScriptAgentMcpServers":                    true,
		"enableAgentDiffs":                               true,
		"enableCsvAttachmentSupport":                     true,
		"showDatabaseAgentsDiscoverability":              true,
		"enableAgentThreadTools":                         false,
		"enableCrdtOperations":                           false,
		"enableAgentCardCustomization":                   true,
		"enableSystemPromptAsPage":                       false,
		"enableUserSessionContext":                       false,
		"enableLargeToolResultComputerOffload":           false,
		"enableScriptAgentGtm":                           false,
		"enablePitCrewTableViewTool":                     false,
		"enableComputer":                                 true,
		"enableCustomAgentCreateGuidanceV2":              true,
		"enableSoftwareFactoryPage":                      false,
		"enableAgentGenerateImage":                       true,
		"enableQueryCalendar":                            false,
		"enableQueryMail":                                false,
		"enableMailExplicitToolCalls":                    true,
		"enableMailNotificationPreferences":              false,
		"enableMailAgentMultiProviderSupport":            true,
		"enableNotionMailDeprecated":                     false,
		"enableWebResearch":                              false,
		"useRulePrioritization":                          true,
		"searchScopes":                                   []map[string]string{{"type": "everything"}},
		"useWebSearch":                                   true,
		"isHipaa":                                        false,
		"internetAccess":                                 false,
		"manageWorkers":                                  false,
		"useReadOnlyMode":                                false,
		"writerMode":                                     false,
		"model":                                          "acai-budino-high",
		"modelFromUser":                                  true,
		"isCustomAgent":                                  false,
		"isCustomAgentBuilder":                           false,
		"isCustomAgentCreate":                            false,
		"isAgentResearchRequest":                         false,
		"useCustomAgentDraft":                            false,
		"enableMarkdownVNext":                            false,
		"enableAgentSkillsV2":                            false,
		"updatePageStaleViewGuardEnabled":                false,
		"enableUpdatePageOrderUpdates":                   true,
		"enableAgentSupportPropertyReorder":              true,
		"enableAgentAskSurvey":                           true,
		"databaseAgentConfigMode":                        false,
		"isOnboardingAgent":                              false,
		"isMobile":                                       false,
	}
	if isSubsequentTurn {
		want["useContextualCoreDocsAutoLoad"] = false
		want["useDocPreviewsForCoreAutoLoad"] = true
	} else {
		want["availableConnectors"] = []interface{}{}
	}
	return want
}

func protocolTestAccount() *Account {
	return &Account{
		UserID:      "user-id",
		UserName:    "Test User",
		UserEmail:   "test@example.com",
		SpaceID:     "space-id",
		SpaceName:   "Test Space",
		SpaceViewID: "space-view-id",
		Timezone:    "Asia/Shanghai",
	}
}

func requireResearcherTranscriptMsg(t *testing.T, value interface{}, wantType string) ResearcherTranscriptMsg {
	t.Helper()
	message, ok := value.(ResearcherTranscriptMsg)
	if !ok {
		t.Fatalf("transcript entry = %#v, want ResearcherTranscriptMsg", value)
	}
	if message.Type != wantType {
		t.Fatalf("transcript entry type = %q, want %q", message.Type, wantType)
	}
	return message
}

func requireTranscriptValueMap(t *testing.T, message ResearcherTranscriptMsg) map[string]interface{} {
	t.Helper()
	value, ok := message.Value.(map[string]interface{})
	if !ok {
		t.Fatalf("transcript value = %#v, want map[string]interface{}", message.Value)
	}
	return value
}
