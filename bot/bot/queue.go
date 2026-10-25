package bot

import "sync"

// chatQueue runs tasks in admission order within each chat. Idle chats leave no
// worker behind; pending tasks wait as metadata rather than blocked goroutines.
// Its zero value is ready to use.
type chatQueue struct {
	mu      sync.Mutex
	pending map[int64][]func()
}

func (q *chatQueue) enqueue(chatID int64, task func()) {
	q.mu.Lock()
	if q.pending == nil {
		q.pending = make(map[int64][]func())
	}
	_, running := q.pending[chatID]
	q.pending[chatID] = append(q.pending[chatID], task)
	q.mu.Unlock()
	if !running {
		go q.run(chatID)
	}
}

func (q *chatQueue) run(chatID int64) {
	for {
		q.mu.Lock()
		tasks := q.pending[chatID]
		if len(tasks) == 0 {
			delete(q.pending, chatID)
			q.mu.Unlock()
			return
		}
		task := tasks[0]
		tasks[0] = nil
		q.pending[chatID] = tasks[1:]
		q.mu.Unlock()
		task()
	}
}
