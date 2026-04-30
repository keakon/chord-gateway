package main

import "time"

func (r *NotificationRouter) ensureReminderState(key string) *reminderState {
	if r.reminders == nil {
		r.reminders = make(map[string]*reminderState)
	}
	st := r.reminders[key]
	if st == nil {
		st = &reminderState{}
		r.reminders[key] = st
	}
	return st
}

// resetReminderTimer arms or resets the reminder timer for the given key.
// Must be called with r.mu held.
func (r *NotificationRouter) resetReminderTimer(key string, st *reminderState, d time.Duration) {
	if d <= 0 {
		d = time.Nanosecond
	}
	if st.timer == nil {
		st.timer = time.AfterFunc(d, func() { r.fireReminder(key) })
	} else {
		st.timer.Reset(d)
	}
}

func reminderDelay(lastPush time.Time, now time.Time) time.Duration {
	if lastPush.IsZero() {
		return reminderInterval
	}
	elapsed := now.Sub(lastPush)
	if elapsed >= reminderInterval {
		return time.Nanosecond
	}
	return reminderInterval - elapsed
}

func (r *NotificationRouter) scheduleReminder(key string, lastPush time.Time) {
	now := time.Now()
	r.mu.Lock()
	st := r.ensureReminderState(key)
	r.resetReminderTimer(key, st, reminderDelay(lastPush, now))
	r.mu.Unlock()
}

func (r *NotificationRouter) beginTurn(key string) {
	now := time.Now()

	if proc, ok := r.lookupProcessByKey(key); ok {
		proc.mu.Lock()
		proc.state.Busy = true
		proc.state.LastPushAt = now
		proc.state.InternalEventsSinceLastPush = 0
		proc.mu.Unlock()
	}

	r.scheduleReminder(key, now)
}

func (r *NotificationRouter) markVisibleOutput(key string) {
	now := time.Now()
	if proc, ok := r.lookupProcessByKey(key); ok {
		proc.mu.Lock()
		proc.state.LastPushAt = now
		proc.state.InternalEventsSinceLastPush = 0
		proc.mu.Unlock()
	}

	r.mu.Lock()
	if st := r.reminders[key]; st != nil {
		r.resetReminderTimer(key, st, reminderInterval)
	}
	r.mu.Unlock()
}

func (r *NotificationRouter) stopReminder(key string) {
	r.mu.Lock()
	if st := r.reminders[key]; st != nil {
		if st.timer != nil {
			st.timer.Stop()
		}
		delete(r.reminders, key)
	}
	r.mu.Unlock()
}

func (r *NotificationRouter) fireReminder(key string) {
	proc, ok := r.lookupProcessByKey(key)
	if !ok || proc == nil || !proc.Alive() {
		r.stopReminder(key)
		return
	}
	state := proc.State()
	if !state.Busy {
		r.stopReminder(key)
		return
	}
	_, _, chatID := parseProcessKey(key)
	if chatID == "" {
		r.stopReminder(key)
		return
	}
	msg := r.formatLongRunningNotification(state)
	if msg != "" {
		r.sendText(chatID, msg)
		r.markVisibleOutput(key)
		return
	}
	r.scheduleReminder(key, state.LastPushAt)
}
