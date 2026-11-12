ALTER TABLE tasks ADD COLUMN developer_agent_id text
    REFERENCES team_agents(id) ON DELETE RESTRICT;
CREATE INDEX tasks_developer_pin ON tasks(developer_agent_id) WHERE developer_agent_id IS NOT NULL;
