package booking

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/sikozonpc/cinema/internal/adapters/redis"
)

func TestConcurrentBooking_ExactlyOneWins(t *testing.T) {
	store := NewRedisStore(redis.NewClient("localhost:6379"))
	svc := NewService(store)

	const numGoroutines = 100_000 // 100k users trying to book a seat at the same time

	var (
		successes atomic.Int64
		failures  atomic.Int64
		wg        sync.WaitGroup
	)

	wg.Add(numGoroutines)
	for i := range numGoroutines {
		go func(userNum int) {
			defer wg.Done()
			_, err := svc.Book(Booking{
				MovieID: "screen-1",
				SeatID:  "A1",
				UserID:  uuid.New().String(),
			})
			if err == nil {
				successes.Add(1)
			} else {
				failures.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if got := successes.Load(); got != 1 {
		t.Errorf("expected exactly 1 success, got %d", got)
	}
	if got := failures.Load(); got != int64(numGoroutines-1) {
		t.Errorf("expected %d failures, got %d", numGoroutines-1, got)
	}
}

func TestBookSeats_RollbackWhenAnySeatTaken(t *testing.T) {
	store := NewRedisStore(redis.NewClient("localhost:6379"))
	svc := NewService(store)
	ctx := context.Background()

	movieID := "rollback-" + uuid.New().String()
	user1 := "user-" + uuid.New().String()
	user2 := "user-" + uuid.New().String()

	if _, err := svc.BookSeats(ctx, movieID, user1, []string{"A1"}); err != nil {
		t.Fatalf("failed initial hold: %v", err)
	}

	_, err := svc.BookSeats(ctx, movieID, user2, []string{"A1", "A2"})
	if err == nil {
		t.Fatalf("expected conflict error for multi-seat hold")
	}
	if err != ErrSeatAlreadyBooked {
		t.Fatalf("expected ErrSeatAlreadyBooked, got %v", err)
	}

	bookings := svc.ListBookings(movieID)
	if len(bookings) != 1 {
		t.Fatalf("expected exactly 1 booking after rollback, got %d", len(bookings))
	}
	if bookings[0].SeatID != "A1" {
		t.Fatalf("expected only A1 to remain booked, got %s", bookings[0].SeatID)
	}
	if bookings[0].UserID != user1 {
		t.Fatalf("expected A1 booking to belong to first user")
	}
}

func TestBookSeats_ManyUsersManyOverlaps_NoPartialSuccess(t *testing.T) {
	store := NewRedisStore(redis.NewClient("localhost:6379"))
	svc := NewService(store)
	ctx := context.Background()

	movieID := "overlap-" + uuid.New().String()

	const (
		numUsers     = 300
		seatPoolSize = 12
		blockSize    = 3
	)

	type successRecord struct {
		SessionID string
		UserID    string
		SeatIDs   []string
	}

	var (
		successes atomic.Int64
		failures  atomic.Int64
		wg        sync.WaitGroup
		mu        sync.Mutex
		records   []successRecord
	)

	buildSeats := func(i int) []string {
		start := (i % (seatPoolSize - blockSize + 1)) + 1
		return []string{
			fmt.Sprintf("A%d", start),
			fmt.Sprintf("A%d", start+1),
			fmt.Sprintf("A%d", start+2),
		}
	}

	wg.Add(numUsers)
	for i := 0; i < numUsers; i++ {
		go func(i int) {
			defer wg.Done()

			userID := "user-" + uuid.New().String()
			session, err := svc.BookSeats(ctx, movieID, userID, buildSeats(i))
			if err != nil {
				failures.Add(1)
				return
			}

			successes.Add(1)
			mu.Lock()
			records = append(records, successRecord{
				SessionID: session.ID,
				UserID:    session.UserID,
				SeatIDs:   append([]string(nil), session.SeatIDs...),
			})
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if successes.Load() == 0 {
		t.Fatalf("expected at least one successful booking")
	}
	if failures.Load() == 0 {
		t.Fatalf("expected some failures with overlapping requests")
	}

	bookings := svc.ListBookings(movieID)
	bySeat := make(map[string]Booking, len(bookings))
	for _, b := range bookings {
		if _, exists := bySeat[b.SeatID]; exists {
			t.Fatalf("seat %s appears more than once", b.SeatID)
		}
		bySeat[b.SeatID] = b
	}

	mu.Lock()
	defer mu.Unlock()
	for _, rec := range records {
		for _, seatID := range rec.SeatIDs {
			b, ok := bySeat[seatID]
			if !ok {
				t.Fatalf("partial success detected: session %s missing seat %s", rec.SessionID, seatID)
			}
			if b.ID != rec.SessionID {
				t.Fatalf("seat %s assigned to different session: want %s got %s", seatID, rec.SessionID, b.ID)
			}
			if b.UserID != rec.UserID {
				t.Fatalf("seat %s assigned to different user: want %s got %s", seatID, rec.UserID, b.UserID)
			}
		}
	}
}
