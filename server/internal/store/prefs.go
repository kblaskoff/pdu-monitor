package store

import "context"

// Preferences of a person (named as in the tables of the second step, see auth.UserKey). The web layer decides which keys and values exist.

func (s *Store) UserPrefs(ctx context.Context, user string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM user_prefs WHERE user = ?`, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) SetUserPref(ctx context.Context, user, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO user_prefs (user, key, value) VALUES (?,?,?) ON CONFLICT(user, key) DO UPDATE SET value = excluded.value`, user, key, value)
	return err
}
