package server

import (
	"errors"
	"net/http"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"time"

	"aunefyren/solstein/feeds"
	"aunefyren/solstein/logger"
	"aunefyren/solstein/models"
	"aunefyren/solstein/rss"

	"github.com/gin-gonic/gin"
)

// The feed page's rule editor (docs/web-ui.md): a feed's rules as one form,
// a row per rule and an empty row for a new one, saved as a whole list as
// the API's PUT does. Reordering is a position select per row rather than
// move buttons, so the form has a single submit button and Enter in a field
// saves instead of moving the first rule.

// uiRuleAction is one choice of the editor's action select.
type uiRuleAction struct {
	Value, Label string
}

// uiRuleActions are the actions offered, as "action" or "action:type".
var uiRuleActions = []uiRuleAction{
	{feeds.RuleHide, "Hide"},
	{feeds.RuleTag + ":full", "Tag as full"},
	{feeds.RuleTag + ":bonus", "Tag as bonus"},
	{feeds.RuleTag + ":trailer", "Tag as trailer"},
}

// uiRule is one row of the rule editor, as stored or as sent.
type uiRule struct {
	// Index is the row's place in the form, from 0; Name is how people
	// read it ("Rule 2", "New rule").
	Index    int
	Name     string
	New      bool
	Position int
	Title    string
	// AtLeast and AtMost are lengths as the page shows them ("20:00").
	AtLeast, AtMost string
	Action          string // a uiRuleActions value, or empty
	Remove          bool
	// Matches is how many episodes the rule decides for ("3 episodes"),
	// for rules as stored; empty for a row not saved yet.
	Matches string
}

// uiRules is the rule editor: the rules as rows, then the row for adding
// one, set apart from them.
type uiRules struct {
	Rows []uiRule
	New  uiRule
	// Positions are what an existing rule can move to; Inserts where the
	// new one can go.
	Positions []int
	Inserts   []uiRuleInsert
	// Saved is how many rules the feed has stored.
	Saved int
}

// uiRuleInsert is one choice of where the new rule goes: before rule
// Position, or at the end when Position is past the last.
type uiRuleInsert struct {
	Position int
	Label    string
}

// Count is how many rows the form sends.
func (rules uiRules) Count() int {
	return len(rules.Rows) + 1
}

// rulesEditor builds the editor from a feed's stored rules, counting what
// each decides for among the episodes.
func rulesEditor(rules []feeds.Rule, list []models.Episode) uiRules {
	counts := feeds.CountMatches(rules, list)
	rows := make([]uiRule, 0, len(rules)+1)
	for i, rule := range rules {
		row := uiRule{
			Index: i, Name: "Rule " + strconv.Itoa(i+1), Position: i + 1,
			Title: rule.TitleMatches, AtLeast: formatLength(rule.MinSeconds), AtMost: formatLength(rule.MaxSeconds),
			Action: rule.Action,
		}
		if rule.Action == feeds.RuleTag {
			row.Action += ":" + rule.EpisodeType
		}
		if i < len(counts) {
			row.Matches = "None"
			if counts[i] > 0 {
				row.Matches = plural(counts[i], "episode")
			}
		}
		rows = append(rows, row)
	}
	rows = append(rows, uiRule{Index: len(rules), Name: "New rule", New: true, Position: len(rules) + 1})
	return newRulesEditor(rows, len(rules))
}

func newRulesEditor(rows []uiRule, saved int) uiRules {
	editor := uiRules{Saved: saved}
	for _, row := range rows {
		if row.New {
			editor.New = row
		} else {
			editor.Rows = append(editor.Rows, row)
		}
	}
	if !editor.New.New {
		count := len(editor.Rows)
		editor.New = uiRule{Index: count, Name: "New rule", New: true, Position: count + 1}
	}
	for i := range editor.Rows {
		editor.Positions = append(editor.Positions, i+1)
		editor.Inserts = append(editor.Inserts, uiRuleInsert{Position: i + 1, Label: "Before rule " + strconv.Itoa(i+1)})
	}
	editor.Inserts = append(editor.Inserts, uiRuleInsert{Position: len(editor.Rows) + 1, Label: "At the end"})
	return editor
}

// rulesFromForm reads the editor's form: the rows as sent, for showing it
// again, and the rules to save in their new order. problem says what is
// wrong with them, in words for the page, when they can't be saved.
func rulesFromForm(context *gin.Context, saved int) (editor uiRules, rules []feeds.Rule, problem string) {
	count, _ := strconv.Atoi(context.PostForm("rules"))
	count = max(0, min(count, feeds.MaxRules+1))
	// Rules are ordered by twice their position, so the new rule, going
	// "before rule n", sorts just ahead of the rule now at n.
	type kept struct {
		order int
		rule  feeds.Rule
	}
	var keep []kept
	var rows []uiRule
	for i := range count {
		field := func(name string) string {
			return strings.TrimSpace(context.PostForm("rule-" + strconv.Itoa(i) + "-" + name))
		}
		row := uiRule{
			Index: i, Name: "Rule " + strconv.Itoa(i+1), New: i >= saved,
			Title: field("title"), AtLeast: field("at-least"), AtMost: field("at-most"),
			Action: field("action"), Remove: field("remove") != "",
		}
		if row.New {
			row.Name = "New rule"
		}
		row.Position, _ = strconv.Atoi(field("position"))
		if row.Position < 1 || row.Position > count {
			row.Position = count // the end
		}
		rows = append(rows, row)
		if row.Remove || (row.Title == "" && row.AtLeast == "" && row.AtMost == "" && row.Action == "") {
			continue // removed, or the new row left empty
		}
		rule, rowProblem := ruleFromRow(row)
		if rowProblem != "" && problem == "" {
			problem = row.Name + ": " + rowProblem
		}
		order := 2 * row.Position
		if row.New {
			order--
		}
		keep = append(keep, kept{order: order, rule: rule})
	}
	editor = newRulesEditor(rows, saved)
	if problem != "" {
		return editor, nil, problem
	}
	if len(keep) > feeds.MaxRules {
		return editor, nil, "A feed can have at most " + strconv.Itoa(feeds.MaxRules) + " rules."
	}
	// Ties keep the order the rows were in, so moving one rule to an
	// occupied position puts it before the rule already there only when it
	// came first.
	slices.SortStableFunc(keep, func(a, b kept) int { return a.order - b.order })
	rules = make([]feeds.Rule, len(keep))
	for i, entry := range keep {
		rules[i] = entry.rule
	}
	return editor, rules, ""
}

// ruleFromRow turns one row into a rule, or says what is wrong with it.
func ruleFromRow(row uiRule) (feeds.Rule, string) {
	rule := feeds.Rule{TitleMatches: row.Title}
	action, episodeType, _ := strings.Cut(row.Action, ":")
	switch {
	case row.Action == "":
		return rule, "choose an action."
	case !slices.ContainsFunc(uiRuleActions, func(choice uiRuleAction) bool { return choice.Value == row.Action }):
		return rule, "that action isn't one of the choices."
	}
	rule.Action, rule.EpisodeType = action, episodeType
	var ok bool
	if rule.MinSeconds, ok = parseLength(row.AtLeast); !ok {
		return rule, "write lengths as 20:00 or 1:00:00."
	}
	if rule.MaxSeconds, ok = parseLength(row.AtMost); !ok {
		return rule, "write lengths as 20:00 or 1:00:00."
	}
	if rule.MaxSeconds > 0 && rule.MinSeconds > rule.MaxSeconds {
		return rule, "at least is longer than at most, so it can't match anything."
	}
	if _, err := regexp.Compile(rule.TitleMatches); err != nil {
		var syntaxErr *syntax.Error
		if errors.As(err, &syntaxErr) {
			return rule, "the title pattern isn't valid (" + syntaxErr.Code.String() + ")."
		}
		return rule, "the title pattern isn't valid."
	}
	return rule, ""
}

// parseLength reads a length as the page writes it, "M:SS" or "H:MM:SS",
// into seconds; empty is 0, no limit. A bare number is refused rather than
// guessed at: "60" could mean seconds or minutes.
func parseLength(text string) (int, bool) {
	if text == "" {
		return 0, true
	}
	if !strings.Contains(text, ":") {
		return 0, false
	}
	duration, ok := rss.ParseDuration(text)
	if !ok {
		return 0, false
	}
	return int(duration.Round(time.Second) / time.Second), true
}

// feedRules saves the feed's rules from the feed page's editor.
func (ui *ui) feedRules(context *gin.Context) {
	feed, ok := ui.loadUIFeed(context)
	if !ok {
		return
	}
	saved, err := ui.handlers.feeds.Rules(context.Request.Context(), feed.ID)
	if err != nil {
		logger.Log.Error("Failed to load the rules of feed '" + feed.Title + "' for the web UI. Error: " + err.Error())
		ui.renderError(context, http.StatusInternalServerError, "Couldn't save the rules", "Something went wrong; the log says what.")
		return
	}
	editor, rules, problem := rulesFromForm(context, len(saved))
	if problem != "" {
		ui.showFeedPageWith(context, http.StatusBadRequest, &uiNotice{Kind: "error", Text: "Couldn't save the rules of '" + feedTitle(feed) + "'. " + problem}, &editor)
		return
	}
	err = ui.handlers.setRules(context.Request.Context(), feed, rules)
	if errors.Is(err, feeds.ErrInvalidSettings) {
		// The form's own checks should have caught it; the API's words, then.
		ui.showFeedPageWith(context, http.StatusBadRequest, &uiNotice{Kind: "error", Text: "Couldn't save the rules of '" + feedTitle(feed) + "': " + err.Error() + "."}, &editor)
		return
	}
	if err != nil {
		logger.Log.Error("Failed to save the rules of feed '" + feed.Title + "' from the web UI. Error: " + err.Error())
		ui.renderError(context, http.StatusInternalServerError, "Couldn't save the rules", "Something went wrong; the log says what.")
		return
	}
	context.Redirect(http.StatusSeeOther, feedPagePath(feed)+"?done=rules#rules")
}
