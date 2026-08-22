package sessionstore

import "sort"

func sortSessions(sessions []*Session) {
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.Before(sessions[j].UpdatedAt)
	})
}
