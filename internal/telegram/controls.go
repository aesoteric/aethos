package telegram

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aesoteric/aethos/internal/agentcatalog"
	"github.com/aesoteric/aethos/internal/session"
)

func (c *Channel) cancelPrompt(ctx context.Context, record session.Record) error {
	if err := c.sessions.Cancel(ctx, record.ID); err != nil {
		if errors.Is(err, session.ErrNoPrompt) {
			_, sendErr := c.client.sendMessage(
				ctx,
				c.settings.Token,
				c.settings.ChatID,
				record.TopicID,
				"No Prompt is currently running.",
			)
			return sendErr
		}
		return err
	}
	_, err := c.client.sendMessage(
		ctx,
		c.settings.Token,
		c.settings.ChatID,
		record.TopicID,
		"Prompt cancelled.",
	)
	return err
}

func telegramCommand(text string) (string, string) {
	command, argument, _ := strings.Cut(strings.TrimSpace(text), " ")
	if name, _, found := strings.Cut(command, "@"); found {
		command = name
	}
	return command, strings.TrimSpace(argument)
}

func (c *Channel) handleAssistant(ctx context.Context, message *message) error {
	command, argument := telegramCommand(message.Text)
	switch command {
	case "/agents":
		return c.sendAgentList(ctx)
	case "/sessions":
		return c.sendSessionList(ctx)
	case "/close":
		return c.closeSession(ctx, argument)
	case "/new":
	default:
		_, err := c.client.sendMessage(ctx, c.settings.Token, c.settings.ChatID, c.assistantTopicID,
			"Send /agents to list installed Agents, /new to create a Session, /sessions to list Sessions, or /close <Session ID> to close one.")
		return err
	}

	workspace, agentID, err := c.sessionSelection(argument)
	if err != nil {
		_, sendErr := c.client.sendMessage(ctx, c.settings.Token, c.settings.ChatID, c.assistantTopicID, err.Error())
		return errors.Join(err, sendErr)
	}
	topic, err := c.client.createForumTopic(ctx, c.settings.Token, c.settings.ChatID, newSessionTopicName)
	if err != nil {
		return fmt.Errorf("create Session Topic: %w", err)
	}
	record, err := c.sessions.Create(ctx, session.Create{
		Agent:     agentID,
		Workspace: workspace,
		Owner: session.Owner{
			Channel: "telegram",
			ID:      strconv.FormatInt(message.From.ID, 10),
		},
		TopicID: topic.MessageThreadID,
	})
	if err != nil {
		cleanupErr := c.client.deleteForumTopic(ctx, c.settings.Token, c.settings.ChatID, topic.MessageThreadID)
		return errors.Join(fmt.Errorf("create Session: %w", err), cleanupErr)
	}
	if _, err := c.client.sendMessage(ctx, c.settings.Token, c.settings.ChatID, record.TopicID,
		"Session ready. Send a Prompt in this Topic."); err != nil {
		return fmt.Errorf("post Session status: %w", err)
	}
	return nil
}

func (c *Channel) sendAgentList(ctx context.Context) error {
	var installed []agentcatalog.InstalledAgent
	if c.settings.Agents != nil {
		var err error
		installed, err = c.settings.Agents.Installed()
		if err != nil {
			return err
		}
	}
	if len(installed) == 0 {
		_, err := c.client.sendMessage(
			ctx, c.settings.Token, c.settings.ChatID, c.assistantTopicID, "No Agents are installed.",
		)
		return err
	}
	var text strings.Builder
	text.WriteString("Installed Agents:")
	for _, installedAgent := range installed {
		fmt.Fprintf(&text, "\n%s — %s (%s)", installedAgent.ID, installedAgent.Name, installedAgent.Type)
	}
	_, err := c.client.sendMessage(
		ctx, c.settings.Token, c.settings.ChatID, c.assistantTopicID, text.String(),
	)
	return err
}

func (c *Channel) sendSessionList(ctx context.Context) error {
	records, err := c.sessions.List(ctx)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		_, err := c.client.sendMessage(
			ctx,
			c.settings.Token,
			c.settings.ChatID,
			c.assistantTopicID,
			"No Sessions.",
		)
		return err
	}
	if _, err := c.client.sendMessage(
		ctx,
		c.settings.Token,
		c.settings.ChatID,
		c.assistantTopicID,
		"Sessions:",
	); err != nil {
		return err
	}
	for _, record := range records {
		name := strings.TrimSpace(record.Name)
		if name == "" {
			name = "(unnamed)"
		}
		text := fmt.Sprintf(
			"%s\nState: %s\nAgent: %s\nName: %s",
			record.ID,
			record.State,
			record.Agent,
			name,
		)
		if _, err := c.client.sendMessage(
			ctx,
			c.settings.Token,
			c.settings.ChatID,
			c.assistantTopicID,
			text,
		); err != nil {
			return err
		}
	}
	return nil
}

func (c *Channel) closeSession(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		_, err := c.client.sendMessage(
			ctx,
			c.settings.Token,
			c.settings.ChatID,
			c.assistantTopicID,
			"Usage: /close <Session ID>",
		)
		return err
	}
	record, err := c.sessions.CloseSession(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_, sendErr := c.client.sendMessage(
				ctx,
				c.settings.Token,
				c.settings.ChatID,
				c.assistantTopicID,
				"Session not found: "+id,
			)
			return sendErr
		}
		if errors.Is(err, session.ErrInvalidTransition) {
			_, sendErr := c.client.sendMessage(
				ctx,
				c.settings.Token,
				c.settings.ChatID,
				c.assistantTopicID,
				"Session is already closed: "+id,
			)
			return sendErr
		}
		return err
	}
	name := strings.TrimSpace(record.Name)
	if name == "" {
		name = record.ID
	}
	_, err = c.client.sendMessage(
		ctx,
		c.settings.Token,
		c.settings.ChatID,
		c.assistantTopicID,
		fmt.Sprintf("Session closed: %s (%s).", name, record.ID),
	)
	return err
}

func (c *Channel) sessionSelection(argument string) (string, string, error) {
	argument = strings.TrimSpace(argument)
	if argument == "" {
		return c.settings.Workspace, c.settings.DefaultAgent, nil
	}
	workspace, agentID, found := strings.Cut(argument, "|")
	if !found {
		return "", "", fmt.Errorf("usage: /new <Workspace> | <installed Agent ID>")
	}
	workspace = strings.TrimSpace(workspace)
	agentID = strings.TrimSpace(agentID)
	if workspace == "" {
		workspace = c.settings.Workspace
	}
	if agentID == "" {
		agentID = c.settings.DefaultAgent
	}
	if !filepath.IsAbs(workspace) {
		return "", "", fmt.Errorf("workspace must be an absolute path")
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return "", "", fmt.Errorf("open Workspace %q: %w", workspace, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("workspace %q is not a directory", workspace)
	}
	return workspace, agentID, nil
}
