package main

import "time"

// resetReminderTimer arms or resets the reminder timer for the given key.
// Must be called with r.mu held.
func (r *NotificationRouter) resetReminderTimer(key string, d time.Duration) {
	if d <= 0 {
		d = time.Nanosecond
	}
	if r.reminders == nil {
		r.reminders = make(map[string]*time.Timer)
	}
	if t := r.reminders[key]; t != nil {
		t.Reset(d)
		return
	}
	r.reminders[key] = time.AfterFunc(d, func() { r.fireReminder(key) })
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
	r.resetReminderTimer(key, reminderDelay(lastPush, now))
	r.mu.Unlock()
}

func (r *NotificationRouter) beginTurn(key string) {
	now := time.Now()
	if proc, ok := r.lookupProcessByKey(key); ok {
		proc.BeginTurn(now)
	}
	r.scheduleReminder(key, now)
}

func (r *NotificationRouter) markVisibleOutput(key string) {
	now := time.Now()
	if proc, ok := r.lookupProcessByKey(key); ok {
		proc.MarkVisibleOutput(now)
	}

	r.mu.Lock()
	if t := r.reminders[key]; t != nil {
		t.Reset(reminderInterval)
	}
	r.mu.Unlock()
}

func (r *NotificationRouter) stopReminder(key string) {
	r.mu.Lock()
	if t := r.reminders[key]; t != nil {
		t.Stop()
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
