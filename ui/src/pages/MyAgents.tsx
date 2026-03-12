import { useState, useEffect, useCallback } from 'react';

interface UserAgent {
  id: string;
  name: string;
  description: string;
  model: string;
  system_prompt: string;
}

const card: React.CSSProperties = {
  background: 'var(--bg-card)',
  border: '1px solid var(--border)',
  borderRadius: 10,
  padding: 20,
  boxShadow: 'var(--shadow)',
};

const inputStyle: React.CSSProperties = {
  padding: '10px 12px',
  borderRadius: 8,
  border: '1px solid var(--border)',
  background: 'var(--bg-primary)',
  color: 'var(--text-primary)',
  fontSize: 14,
  outline: 'none',
  width: '100%',
  boxSizing: 'border-box',
};

function MyAgents() {
  const [agents, setAgents] = useState<UserAgent[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<UserAgent | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [form, setForm] = useState({ name: '', description: '', model: '', system_prompt: '' });
  const [saving, setSaving] = useState(false);

  const fetchAgents = useCallback(() => {
    fetch('/api/user/agents')
      .then((res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        return res.json();
      })
      .then((data) => setAgents(Array.isArray(data) ? data : []))
      .catch((err) => setError(err.message));
  }, []);

  useEffect(() => { fetchAgents(); }, [fetchAgents]);

  const handleCreate = async () => {
    if (!form.name.trim()) return;
    setSaving(true);
    try {
      const res = await fetch('/api/user/agents', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(form),
      });
      if (!res.ok) {
        const data = await res.json();
        setError(data.error || 'Failed to create agent');
        return;
      }
      setForm({ name: '', description: '', model: '', system_prompt: '' });
      setShowCreate(false);
      fetchAgents();
    } catch (err: any) {
      setError(err.message);
    } finally {
      setSaving(false);
    }
  };

  const handleUpdate = async () => {
    if (!editing) return;
    setSaving(true);
    try {
      const res = await fetch(`/api/user/agents/${editing.name}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(form),
      });
      if (!res.ok) {
        const data = await res.json();
        setError(data.error || 'Failed to update agent');
        return;
      }
      setEditing(null);
      fetchAgents();
    } catch (err: any) {
      setError(err.message);
    } finally {
      setSaving(false);
    }
  };

  const handleDelete = async (name: string) => {
    if (!confirm(`Delete agent "${name}"?`)) return;
    try {
      await fetch(`/api/user/agents/${name}`, { method: 'DELETE' });
      fetchAgents();
    } catch (err: any) {
      setError(err.message);
    }
  };

  const startEdit = (agent: UserAgent) => {
    setEditing(agent);
    setShowCreate(false);
    setForm({
      name: agent.name,
      description: agent.description,
      model: agent.model,
      system_prompt: agent.system_prompt,
    });
  };

  const startCreate = () => {
    setShowCreate(true);
    setEditing(null);
    setForm({ name: '', description: '', model: '', system_prompt: '' });
  };

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 28 }}>
        <h1 style={{ fontSize: 28, fontWeight: 700, color: 'var(--text-primary)', margin: 0 }}>My Agents</h1>
        <button
          onClick={startCreate}
          style={{
            padding: '8px 20px',
            background: 'var(--accent)',
            color: '#fff',
            border: 'none',
            borderRadius: 8,
            cursor: 'pointer',
            fontSize: 14,
            fontWeight: 600,
          }}
        >
          + New Agent
        </button>
      </div>

      {error && (
        <div style={{ ...card, color: 'var(--red-light)', marginBottom: 16 }}>
          {error}
          <button onClick={() => setError(null)} style={{ marginLeft: 12, background: 'none', border: 'none', color: 'var(--text-secondary)', cursor: 'pointer' }}>dismiss</button>
        </div>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(300px, 1fr))', gap: 16 }}>
        {agents.map((agent) => (
          <div key={agent.id} style={card}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
              <span style={{ fontSize: 18, fontWeight: 600, color: 'var(--text-primary)' }}>@{agent.name}</span>
              <div style={{ display: 'flex', gap: 8 }}>
                <button
                  onClick={() => startEdit(agent)}
                  style={{ background: 'none', border: 'none', cursor: 'pointer', color: 'var(--text-secondary)', fontSize: 14 }}
                >
                  Edit
                </button>
                <button
                  onClick={() => handleDelete(agent.name)}
                  style={{ background: 'none', border: 'none', cursor: 'pointer', color: '#ef4444', fontSize: 14 }}
                >
                  Delete
                </button>
              </div>
            </div>
            {agent.description && (
              <div style={{ fontSize: 15, color: 'var(--text-tertiary)', marginBottom: 4 }}>{agent.description}</div>
            )}
            {agent.model && (
              <div style={{ fontSize: 14, color: 'var(--text-muted)' }}>Model: {agent.model}</div>
            )}
          </div>
        ))}
      </div>

      {agents.length === 0 && !error && (
        <div style={{ color: 'var(--text-tertiary)', fontSize: 16, marginTop: 16 }}>
          No agents yet. Create one to get started.
        </div>
      )}

      {(showCreate || editing) && (
        <div style={{ ...card, marginTop: 24 }}>
          <h2 style={{ fontSize: 20, fontWeight: 600, marginBottom: 16, color: 'var(--text-primary)' }}>
            {editing ? `Edit ${editing.name}` : 'New Agent'}
          </h2>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
            {!editing && (
              <div>
                <label style={{ fontSize: 14, color: 'var(--text-secondary)', marginBottom: 4, display: 'block' }}>Name</label>
                <input
                  style={inputStyle}
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                  placeholder="e.g. coder, writer, researcher"
                />
              </div>
            )}
            <div>
              <label style={{ fontSize: 14, color: 'var(--text-secondary)', marginBottom: 4, display: 'block' }}>Description</label>
              <input
                style={inputStyle}
                value={form.description}
                onChange={(e) => setForm({ ...form, description: e.target.value })}
                placeholder="What this agent does"
              />
            </div>
            <div>
              <label style={{ fontSize: 14, color: 'var(--text-secondary)', marginBottom: 4, display: 'block' }}>Model</label>
              <input
                style={inputStyle}
                value={form.model}
                onChange={(e) => setForm({ ...form, model: e.target.value })}
                placeholder="e.g. claude-sonnet-4-5-20250929 (leave empty for default)"
              />
            </div>
            <div>
              <label style={{ fontSize: 14, color: 'var(--text-secondary)', marginBottom: 4, display: 'block' }}>System Prompt</label>
              <textarea
                style={{ ...inputStyle, minHeight: 120, fontFamily: 'monospace', resize: 'vertical' }}
                value={form.system_prompt}
                onChange={(e) => setForm({ ...form, system_prompt: e.target.value })}
                placeholder="Instructions for this agent's behavior and expertise"
              />
            </div>
            <div style={{ display: 'flex', gap: 12, marginTop: 4 }}>
              <button
                onClick={editing ? handleUpdate : handleCreate}
                disabled={saving || (!editing && !form.name.trim())}
                style={{
                  padding: '8px 24px',
                  background: 'var(--accent)',
                  color: '#fff',
                  border: 'none',
                  borderRadius: 8,
                  cursor: 'pointer',
                  fontSize: 14,
                  fontWeight: 600,
                  opacity: saving ? 0.6 : 1,
                }}
              >
                {saving ? 'Saving...' : editing ? 'Update' : 'Create'}
              </button>
              <button
                onClick={() => { setShowCreate(false); setEditing(null); }}
                style={{
                  padding: '8px 24px',
                  background: 'transparent',
                  color: 'var(--text-secondary)',
                  border: '1px solid var(--border)',
                  borderRadius: 8,
                  cursor: 'pointer',
                  fontSize: 14,
                }}
              >
                Cancel
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

export default MyAgents;
