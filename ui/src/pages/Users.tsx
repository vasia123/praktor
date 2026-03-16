import { useState, useEffect, useCallback } from 'react';

interface User {
  id: string;
  username: string;
  display_name: string;
  is_admin: boolean;
  has_password: boolean;
  status: string;
  telegram_id: number;
  created_at: string;
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

const badge = (color: string, bg: string): React.CSSProperties => ({
  display: 'inline-block',
  padding: '2px 10px',
  borderRadius: 999,
  fontSize: 14,
  fontWeight: 600,
  background: bg,
  color,
});

const statusBadge = (status: string): React.CSSProperties => {
  switch (status) {
    case 'pending':
      return badge('#f59e0b', 'rgba(245,158,11,0.15)');
    case 'approved':
      return badge('#22c55e', 'rgba(34,197,94,0.15)');
    case 'blocked':
      return badge('#ef4444', 'rgba(239,68,68,0.15)');
    default:
      return badge('var(--text-secondary)', 'rgba(128,128,128,0.15)');
  }
};

const smallBtn = (color: string): React.CSSProperties => ({
  background: 'none',
  border: 'none',
  cursor: 'pointer',
  color,
  fontSize: 14,
});

function Users() {
  const [users, setUsers] = useState<User[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [showCreate, setShowCreate] = useState(false);
  const [form, setForm] = useState({ username: '', display_name: '', password: '', is_admin: false });
  const [pwUser, setPwUser] = useState<User | null>(null);
  const [newPw, setNewPw] = useState('');
  const [saving, setSaving] = useState(false);

  const fetchUsers = useCallback(() => {
    fetch('/api/admin/users')
      .then((res) => {
        if (res.status === 403) throw new Error('Admin access required');
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        return res.json();
      })
      .then((data) => {
        const list: User[] = Array.isArray(data) ? data : [];
        // Sort: pending first, then approved, then blocked
        const order: Record<string, number> = { pending: 0, approved: 1, blocked: 2 };
        list.sort((a, b) => (order[a.status] ?? 3) - (order[b.status] ?? 3));
        setUsers(list);
      })
      .catch((err) => setError(err.message));
  }, []);

  useEffect(() => { fetchUsers(); }, [fetchUsers]);

  const handleCreate = async () => {
    if (!form.username.trim()) return;
    setSaving(true);
    try {
      const res = await fetch('/api/admin/users', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(form),
      });
      if (!res.ok) {
        const data = await res.json();
        setError(data.error || 'Failed to create user');
        return;
      }
      setForm({ username: '', display_name: '', password: '', is_admin: false });
      setShowCreate(false);
      fetchUsers();
    } catch (err: any) {
      setError(err.message);
    } finally {
      setSaving(false);
    }
  };

  const handleSetPassword = async () => {
    if (!pwUser || !newPw) return;
    setSaving(true);
    try {
      const res = await fetch(`/api/admin/users/${pwUser.id}/password`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password: newPw }),
      });
      if (!res.ok) {
        const data = await res.json();
        setError(data.error || 'Failed to set password');
        return;
      }
      setPwUser(null);
      setNewPw('');
      fetchUsers();
    } catch (err: any) {
      setError(err.message);
    } finally {
      setSaving(false);
    }
  };

  const handleStatusChange = async (userId: string, status: string) => {
    try {
      const res = await fetch(`/api/admin/users/${userId}/status`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ status }),
      });
      if (!res.ok) {
        const data = await res.json();
        setError(data.error || 'Failed to update status');
        return;
      }
      fetchUsers();
    } catch (err: any) {
      setError(err.message);
    }
  };

  const handleDelete = async (user: User) => {
    if (!confirm(`Delete user "${user.username}"? This will also delete their agents.`)) return;
    try {
      const res = await fetch(`/api/admin/users/${user.id}`, { method: 'DELETE' });
      if (!res.ok) {
        const data = await res.json();
        setError(data.error || 'Failed to delete user');
        return;
      }
      fetchUsers();
    } catch (err: any) {
      setError(err.message);
    }
  };

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 28 }}>
        <h1 style={{ fontSize: 28, fontWeight: 700, color: 'var(--text-primary)', margin: 0 }}>Users</h1>
        <button
          onClick={() => { setShowCreate(true); setPwUser(null); }}
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
          + New User
        </button>
      </div>

      {error && (
        <div style={{ ...card, color: 'var(--red-light)', marginBottom: 16 }}>
          {error}
          <button onClick={() => setError(null)} style={{ marginLeft: 12, background: 'none', border: 'none', color: 'var(--text-secondary)', cursor: 'pointer' }}>dismiss</button>
        </div>
      )}

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(300px, 1fr))', gap: 16 }}>
        {users.map((user) => (
          <div key={user.id} style={card}>
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
              <span style={{ fontSize: 18, fontWeight: 600, color: 'var(--text-primary)' }}>{user.username}</span>
              <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                <span style={statusBadge(user.status)}>{user.status}</span>
                {user.is_admin && (
                  <span style={badge('var(--accent)', 'var(--accent-muted)')}>admin</span>
                )}
                {!user.has_password && (
                  <span style={badge('#f59e0b', 'rgba(245,158,11,0.15)')}>no password</span>
                )}
              </div>
            </div>
            {user.display_name && (
              <div style={{ fontSize: 15, color: 'var(--text-tertiary)', marginBottom: 4 }}>{user.display_name}</div>
            )}
            <div style={{ fontSize: 14, color: 'var(--text-muted)', marginBottom: 8 }}>
              ID: {user.id}
              {user.telegram_id > 0 && <span> &middot; TG: {user.telegram_id}</span>}
            </div>
            <div style={{ display: 'flex', gap: 12, flexWrap: 'wrap' }}>
              {user.status !== 'approved' && (
                <button onClick={() => handleStatusChange(user.id, 'approved')} style={smallBtn('#22c55e')}>
                  Approve
                </button>
              )}
              {user.status !== 'blocked' && (
                <button onClick={() => handleStatusChange(user.id, 'blocked')} style={smallBtn('#ef4444')}>
                  Block
                </button>
              )}
              <button
                onClick={() => { setPwUser(user); setShowCreate(false); setNewPw(''); }}
                style={smallBtn('var(--text-secondary)')}
              >
                Set Password
              </button>
              <button
                onClick={() => handleDelete(user)}
                style={smallBtn('#ef4444')}
              >
                Delete
              </button>
            </div>
          </div>
        ))}
      </div>

      {showCreate && (
        <div style={{ ...card, marginTop: 24 }}>
          <h2 style={{ fontSize: 20, fontWeight: 600, marginBottom: 16, color: 'var(--text-primary)' }}>New User</h2>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
            <div>
              <label style={{ fontSize: 14, color: 'var(--text-secondary)', marginBottom: 4, display: 'block' }}>Username</label>
              <input
                style={inputStyle}
                value={form.username}
                onChange={(e) => setForm({ ...form, username: e.target.value })}
                placeholder="Username for login"
              />
            </div>
            <div>
              <label style={{ fontSize: 14, color: 'var(--text-secondary)', marginBottom: 4, display: 'block' }}>Display Name</label>
              <input
                style={inputStyle}
                value={form.display_name}
                onChange={(e) => setForm({ ...form, display_name: e.target.value })}
                placeholder="Display name (optional)"
              />
            </div>
            <div>
              <label style={{ fontSize: 14, color: 'var(--text-secondary)', marginBottom: 4, display: 'block' }}>Password</label>
              <input
                type="password"
                style={inputStyle}
                value={form.password}
                onChange={(e) => setForm({ ...form, password: e.target.value })}
                placeholder="Password for web login"
              />
            </div>
            <div>
              <label style={{ display: 'flex', alignItems: 'center', gap: 8, fontSize: 14, color: 'var(--text-secondary)', cursor: 'pointer' }}>
                <input
                  type="checkbox"
                  checked={form.is_admin}
                  onChange={(e) => setForm({ ...form, is_admin: e.target.checked })}
                />
                Admin privileges
              </label>
            </div>
            <div style={{ display: 'flex', gap: 12, marginTop: 4 }}>
              <button
                onClick={handleCreate}
                disabled={saving || !form.username.trim()}
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
                {saving ? 'Creating...' : 'Create'}
              </button>
              <button
                onClick={() => setShowCreate(false)}
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

      {pwUser && (
        <div style={{ ...card, marginTop: 24 }}>
          <h2 style={{ fontSize: 20, fontWeight: 600, marginBottom: 16, color: 'var(--text-primary)' }}>
            Set Password for {pwUser.username}
          </h2>
          <div style={{ display: 'flex', gap: 12, alignItems: 'center' }}>
            <input
              type="password"
              style={{ ...inputStyle, flex: 1 }}
              value={newPw}
              onChange={(e) => setNewPw(e.target.value)}
              placeholder="New password"
            />
            <button
              onClick={handleSetPassword}
              disabled={saving || !newPw}
              style={{
                padding: '10px 24px',
                background: 'var(--accent)',
                color: '#fff',
                border: 'none',
                borderRadius: 8,
                cursor: 'pointer',
                fontSize: 14,
                fontWeight: 600,
                opacity: saving ? 0.6 : 1,
                whiteSpace: 'nowrap',
              }}
            >
              {saving ? 'Saving...' : 'Set'}
            </button>
            <button
              onClick={() => setPwUser(null)}
              style={{
                padding: '10px 24px',
                background: 'transparent',
                color: 'var(--text-secondary)',
                border: '1px solid var(--border)',
                borderRadius: 8,
                cursor: 'pointer',
                fontSize: 14,
                whiteSpace: 'nowrap',
              }}
            >
              Cancel
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

export default Users;
