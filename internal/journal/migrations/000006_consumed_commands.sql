ALTER TABLE agent_commands DROP CONSTRAINT agent_commands_terminal_reason_check;
ALTER TABLE agent_commands ADD CHECK (terminal_reason IN ('','failed','auth_required','interrupted','consumed'));
