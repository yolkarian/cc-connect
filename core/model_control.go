package core

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const nativeModelPageSize = 18

func usesSessionModelControls(agent Agent) bool {
	provider, ok := agent.(SessionModelControlProvider)
	return ok && provider.SupportsSessionModelControl()
}

func (e *Engine) sessionModelController(sessionKey string, p Platform, replyCtx any) (SessionModelController, error) {
	agent, sessions := e.sessionContextForKey(sessionKey)
	return e.sessionModelControllerWithContext(sessionKey, p, replyCtx, agent, sessions, e.interactiveKeyForSessionKey(sessionKey))
}

func (e *Engine) sessionModelControllerWithContext(sessionKey string, p Platform, replyCtx any, agent Agent, sessions *SessionManager, key string) (SessionModelController, error) {
	if err := e.nativeModelMigrationError(); err != nil {
		return nil, err
	}
	if p == nil {
		p = e.platformForName(extractPlatformName(sessionKey))
	}
	session := sessions.GetOrCreateActive(sessionKey)
	state := e.getOrCreateInteractiveStateWith(key, p, replyCtx, session, sessions, agent, sessionKey)
	if state.startError != nil {
		return nil, state.startError
	}
	controller, ok := state.agentSession.(SessionModelController)
	if !ok {
		return nil, fmt.Errorf("session does not expose native model controls")
	}
	return controller, nil
}

func modelChoiceToken(name string) string {
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("@%x", sum[:6])
}

func resolveNativeModelTarget(input string, models []ModelOption) string {
	if strings.HasPrefix(input, "@") {
		var target string
		for _, model := range models {
			if modelChoiceToken(model.Name) == input {
				if target != "" {
					return input // ambiguous: backend validation must reject it
				}
				target = model.Name
			}
		}
		if target != "" {
			return target
		}
	}
	return resolveModelSwitchTarget(input, models)
}

func (e *Engine) switchNativeModel(sessionKey, target string, p Platform, replyCtx any) (string, error) {
	controller, err := e.sessionModelController(sessionKey, p, replyCtx)
	if err != nil {
		return target, err
	}
	target = resolveNativeModelTarget(target, controller.ModelControls().Models)
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	if err := controller.SetSessionModel(ctx, target); err != nil {
		return target, err
	}
	actual := controller.ModelControls()
	if actual.CurrentModel != target {
		return target, fmt.Errorf("backend did not confirm model %q", target)
	}
	// The backend owns both selection and persistence, including any effort
	// clamp. Never copy its live selection into constructor/workspace defaults.
	return target, nil
}

func (e *Engine) switchNativeReasoning(sessionKey, input string, p Platform, replyCtx any) (string, error) {
	controller, err := e.sessionModelController(sessionKey, p, replyCtx)
	if err != nil {
		return input, err
	}
	choices := controller.ModelControls().ReasoningEfforts
	target := strings.ToLower(strings.TrimSpace(input))
	if index, err := strconv.Atoi(target); err == nil && index > 0 && index <= len(choices) {
		target = choices[index-1]
	}
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	if err := controller.SetSessionReasoningEffort(ctx, target); err != nil {
		return target, err
	}
	actual := controller.ModelControls().CurrentReasoningEffort
	if actual != target {
		return target, fmt.Errorf("backend did not confirm reasoning effort %q", target)
	}
	return actual, nil
}

func (e *Engine) nativeModelMigrationError() error {
	if usesSessionModelControls(e.agent) && e.projectState != nil && e.projectState.HasLegacyNativeModelOverrides() {
		return fmt.Errorf("%s", e.i18n.Tf(MsgNativeModelMigrationNeeded, e.projectState.storePath))
	}
	return nil
}

func nativeModelPage(args []string) int {
	if len(args) == 2 && args[0] == "page" {
		if page, err := strconv.Atoi(args[1]); err == nil && page > 0 {
			return page
		}
	}
	return 0 // first open: show the page containing the active model
}

func currentModelPage(controls ModelControls) int {
	for i, model := range controls.Models {
		if model.Name == controls.CurrentModel {
			return i/nativeModelPageSize + 1
		}
	}
	return 1
}

func modelPageBounds(models []ModelOption, page int) (int, int, int) {
	pages := max(1, (len(models)+nativeModelPageSize-1)/nativeModelPageSize)
	page = max(1, min(page, pages))
	start := (page - 1) * nativeModelPageSize
	return start, min(start+nativeModelPageSize, len(models)), pages
}

func (e *Engine) cmdNativeModel(p Platform, msg *Message, args []string) {
	if len(args) > 0 && args[0] != "page" {
		target, ok := parseModelSwitchArgs(args)
		if !ok {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgModelUsage))
			return
		}
		actual, err := e.switchNativeModel(msg.SessionKey, target, p, msg.ReplyCtx)
		if err != nil {
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgModelChangeFailed, err))
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgModelChanged, actual))
		}
		return
	}
	controller, err := e.sessionModelController(msg.SessionKey, p, msg.ReplyCtx)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgModelChangeFailed, err))
		return
	}
	controls := controller.ModelControls()
	if len(controls.Models) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgModelNotSupported))
		return
	}
	page := nativeModelPage(args)
	if page == 0 {
		page = currentModelPage(controls)
	}
	if supportsCards(p) {
		e.replyWithCard(p, msg.ReplyCtx, e.nativeModelOptionsCard(controls, page))
		return
	}
	start, end, pages := modelPageBounds(controls.Models, page)
	var sb strings.Builder
	sb.WriteString(e.i18n.Tf(MsgModelCurrent, controls.CurrentModel))
	sb.WriteString("\n\n")
	sb.WriteString(e.i18n.T(MsgModelListTitle))
	var buttons [][]ButtonOption
	var row []ButtonOption
	for i := start; i < end; i++ {
		model := controls.Models[i]
		label := model.Name
		if model.Alias != "" {
			label = model.Alias
		}
		marker := "  "
		if model.Name == controls.CurrentModel {
			marker = "> "
			label = "▶ " + label
		}
		fmt.Fprintf(&sb, "%s%d. %s\n", marker, i+1, model.Name)
		row = append(row, ButtonOption{Text: label, Data: "cmd:/model switch " + modelChoiceToken(model.Name)})
		if len(row) == 3 {
			buttons = append(buttons, row)
			row = nil
		}
	}
	if len(row) > 0 {
		buttons = append(buttons, row)
	}
	page = start/nativeModelPageSize + 1
	var nav []ButtonOption
	if page > 1 {
		nav = append(nav, ButtonOption{Text: e.i18n.T(MsgCardPrev), Data: fmt.Sprintf("cmd:/model page %d", page-1)})
	}
	if page < pages {
		nav = append(nav, ButtonOption{Text: e.i18n.T(MsgCardNext), Data: fmt.Sprintf("cmd:/model page %d", page+1)})
	}
	if len(nav) > 0 {
		buttons = append(buttons, nav)
	}
	sb.WriteString("\n")
	sb.WriteString(e.i18n.T(MsgModelUsage))
	e.replyWithButtons(p, msg.ReplyCtx, sb.String(), buttons)
}

func (e *Engine) nativeModelOptionsCard(controls ModelControls, page int) *Card {
	start, end, pages := modelPageBounds(controls.Models, page)
	var options []CardSelectOption
	current := ""
	for _, model := range controls.Models[start:end] {
		value := "act:/model switch " + modelChoiceToken(model.Name)
		options = append(options, CardSelectOption{Text: model.Name, Value: value})
		if model.Name == controls.CurrentModel {
			current = value
		}
	}
	cb := NewCard().Title(e.i18n.T(MsgCardTitleModel), "indigo").
		Markdown(e.i18n.Tf(MsgModelCurrent, controls.CurrentModel)).
		Select(e.i18n.T(MsgModelSelectPlaceholder), options, current)
	page = start/nativeModelPageSize + 1
	var buttons []CardButton
	if page > 1 {
		buttons = append(buttons, CardButton{Text: e.i18n.T(MsgCardPrev), Value: fmt.Sprintf("cmd:/model page %d", page-1)})
	}
	if page < pages {
		buttons = append(buttons, CardButton{Text: e.i18n.T(MsgCardNext), Value: fmt.Sprintf("cmd:/model page %d", page+1)})
	}
	buttons = append(buttons, e.cardBackButton())
	cb.Buttons(buttons...).Note(e.i18n.T(MsgModelUsage))
	return cb.Build()
}

func (e *Engine) renderNativeModelCard(sessionKey string, page int) *Card {
	controller, err := e.sessionModelController(sessionKey, nil, nil)
	if err != nil {
		return e.renderModelSwitchResultCard("", err)
	}
	controls := controller.ModelControls()
	if len(controls.Models) == 0 {
		return e.simpleCard(e.i18n.T(MsgCardTitleModel), "indigo", e.i18n.T(MsgModelNotSupported))
	}
	if page == 0 {
		page = currentModelPage(controls)
	}
	return e.nativeModelOptionsCard(controls, page)
}

func (e *Engine) cmdNativeReasoning(p Platform, msg *Message, args []string) {
	if len(args) > 0 {
		actual, err := e.switchNativeReasoning(msg.SessionKey, args[0], p, msg.ReplyCtx)
		if err != nil {
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgReasoningChangeFailed, err))
		} else {
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgReasoningChanged, actual))
		}
		return
	}
	controller, err := e.sessionModelController(msg.SessionKey, p, msg.ReplyCtx)
	if err != nil {
		e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgReasoningChangeFailed, err))
		return
	}
	controls := controller.ModelControls()
	if len(controls.ReasoningEfforts) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgReasoningNotSupported))
		return
	}
	if supportsCards(p) {
		e.replyWithCard(p, msg.ReplyCtx, e.nativeReasoningOptionsCard(controls))
		return
	}
	var sb strings.Builder
	sb.WriteString(e.i18n.Tf(MsgReasoningCurrent, controls.CurrentReasoningEffort))
	sb.WriteString("\n\n")
	sb.WriteString(e.i18n.T(MsgReasoningListTitle))
	var buttons [][]ButtonOption
	for i, effort := range controls.ReasoningEfforts {
		fmt.Fprintf(&sb, "%d. %s\n", i+1, effort)
		buttons = append(buttons, []ButtonOption{{Text: effort, Data: "cmd:/reasoning " + effort}})
	}
	sb.WriteString("\n")
	sb.WriteString(e.reasoningUsage(controls.ReasoningEfforts))
	e.replyWithButtons(p, msg.ReplyCtx, sb.String(), buttons)
}

func (e *Engine) nativeReasoningOptionsCard(controls ModelControls) *Card {
	var options []CardSelectOption
	current := ""
	for _, effort := range controls.ReasoningEfforts {
		value := "act:/reasoning " + effort
		options = append(options, CardSelectOption{Text: effort, Value: value})
		if effort == controls.CurrentReasoningEffort {
			current = value
		}
	}
	return NewCard().Title(e.i18n.T(MsgCardTitleReasoning), "orange").
		Markdown(e.i18n.Tf(MsgReasoningCurrent, controls.CurrentReasoningEffort)).
		Select(e.i18n.T(MsgReasoningSelectPlaceholder), options, current).
		Buttons(e.cardBackButton()).Note(e.reasoningUsage(controls.ReasoningEfforts)).Build()
}

func (e *Engine) renderNativeReasoningCard(sessionKey string) *Card {
	controller, err := e.sessionModelController(sessionKey, nil, nil)
	if err != nil {
		return e.simpleCard(e.i18n.T(MsgCardTitleReasoning), "red", e.i18n.Tf(MsgReasoningChangeFailed, err))
	}
	controls := controller.ModelControls()
	if len(controls.ReasoningEfforts) == 0 {
		return e.simpleCard(e.i18n.T(MsgCardTitleReasoning), "orange", e.i18n.T(MsgReasoningNotSupported))
	}
	return e.nativeReasoningOptionsCard(controls)
}
