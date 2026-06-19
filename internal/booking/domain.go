package booking

import (
	"context"
	"errors"
	"time"
)

var (
	ErrSeatAlreadyBooked  = errors.New("seat is already taken")
	ErrInvalidSeatRequest = errors.New("invalid seat request")
)

// Booking represents a confirmed seat reservation.
type Booking struct {
	ID        string
	MovieID   string
	SeatID    string
	SeatIDs   []string
	UserID    string
	Status    string
	ExpiresAt time.Time
}

type BookingStore interface {
	Book(b Booking) (Booking, error)
	BookMany(ctx context.Context, movieID string, userID string, seatIDs []string) (Booking, error)
	ListBookings(movieID string) []Booking

	Confirm(ctx context.Context, sessionID string, userID string) (Booking, error)
	Release(ctx context.Context, sessionID string, userID string) error
}
