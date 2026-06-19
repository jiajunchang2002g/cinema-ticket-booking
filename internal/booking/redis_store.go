package booking

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const defaultHoldTTL = 2 * time.Minute

var holdManySeatsScript = redis.NewScript(`
for i = 1, (#KEYS - 1) do
	if redis.call('EXISTS', KEYS[i]) == 1 then
		return 0
	end
end
for i = 1, (#KEYS - 1) do
	redis.call('SET', KEYS[i], ARGV[i + 2], 'EX', ARGV[1])
end
redis.call('SET', KEYS[#KEYS], ARGV[2], 'EX', ARGV[1])
return 1
`)

var confirmManySeatsScript = redis.NewScript(`
for i = 1, (#KEYS - 1) do
	if redis.call('EXISTS', KEYS[i]) == 0 then
		return 0
	end
end
for i = 1, (#KEYS - 1) do
	redis.call('SET', KEYS[i], ARGV[i + 1])
end
redis.call('SET', KEYS[#KEYS], ARGV[1])
return 1
`)

// RedisStore implements session-based seat booking backed by Redis.
//
// Key design:
//
//	seat:{movieID}:{seatID}   → session JSON (TTL = held, no TTL = confirmed)
//	session:{sessionID}       → seat key     (reverse lookup)
type RedisStore struct {
	rdb *redis.Client
}

func NewRedisStore(rdb *redis.Client) *RedisStore {
	return &RedisStore{rdb: rdb}
}

// sessionKey builds the reverse-lookup key for a session.
func sessionKey(id string) string {
	return fmt.Sprintf("session:%s", id)
}

func seatKey(movieID string, seatID string) string {
	return fmt.Sprintf("seat:%s:%s", movieID, seatID)
}

func marshalBookingPayload(b Booking) string {
	payload, _ := json.Marshal(b)
	return string(payload)
}

func buildSeatKeys(movieID string, seatIDs []string) []string {
	keys := make([]string, 0, len(seatIDs))
	for _, seatID := range seatIDs {
		keys = append(keys, seatKey(movieID, seatID))
	}
	return keys
}

func buildHeldSession(movieID string, userID string, seatIDs []string) Booking {
	now := time.Now()
	return Booking{
		ID:        uuid.New().String(),
		MovieID:   movieID,
		SeatID:    seatIDs[0],
		SeatIDs:   seatIDs,
		UserID:    userID,
		Status:    "held",
		ExpiresAt: now.Add(defaultHoldTTL),
	}
}

func buildSeatPayloads(session Booking, status string) []string {
	payloads := make([]string, 0, len(session.SeatIDs))
	for _, seatID := range session.SeatIDs {
		payloads = append(payloads, marshalBookingPayload(Booking{
			ID:      session.ID,
			MovieID: session.MovieID,
			SeatID:  seatID,
			UserID:  session.UserID,
			Status:  status,
		}))
	}
	return payloads
}

func (s *RedisStore) Book(b Booking) (Booking, error) {
	return s.BookMany(context.Background(), b.MovieID, b.UserID, []string{b.SeatID})
}

func normalizeSeatIDs(seatIDs []string) []string {
	seen := make(map[string]struct{}, len(seatIDs))
	out := make([]string, 0, len(seatIDs))
	for _, seat := range seatIDs {
		trimmed := strings.TrimSpace(seat)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

func (s *RedisStore) BookMany(ctx context.Context, movieID string, userID string, seatIDs []string) (Booking, error) {
	if userID == "" || movieID == "" {
		return Booking{}, ErrInvalidSeatRequest
	}

	seatIDs = normalizeSeatIDs(seatIDs)
	if len(seatIDs) == 0 {
		return Booking{}, ErrInvalidSeatRequest
	}

	session := buildHeldSession(movieID, userID, seatIDs)
	seatKeys := buildSeatKeys(movieID, session.SeatIDs)
	seatPayloads := buildSeatPayloads(session, "held")

	result, err := s.runHoldMany(ctx, session, seatKeys, seatPayloads)
	if err != nil {
		return Booking{}, err
	}
	if result != 1 {
		return Booking{}, ErrSeatAlreadyBooked
	}

	log.Printf("Session booked %v", session)
	return session, nil
}

func (s *RedisStore) runHoldMany(ctx context.Context, session Booking, seatKeys []string, seatPayloads []string) (int, error) {
	keys := make([]string, 0, len(seatKeys)+1)
	keys = append(keys, seatKeys...)
	keys = append(keys, sessionKey(session.ID))

	args := []interface{}{int(defaultHoldTTL.Seconds()), marshalBookingPayload(session)}
	for _, val := range seatPayloads {
		args = append(args, val)
	}

	return holdManySeatsScript.Run(ctx, s.rdb, keys, args...).Int()
}

func (s *RedisStore) ListBookings(movieID string) []Booking {
	pattern := fmt.Sprintf("seat:%s:*", movieID)
	var sessions []Booking

	ctx := context.Background()

	iter := s.rdb.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		val, err := s.rdb.Get(ctx, iter.Val()).Result()
		if err != nil {
			continue
		}
		session, err := parseSession(val)
		if err != nil {
			continue
		}
		sessions = append(sessions, session)
	}

	return sessions
}

func parseSession(val string) (Booking, error) {
	var data Booking
	if err := json.Unmarshal([]byte(val), &data); err != nil {
		return Booking{}, err
	}
	if len(data.SeatIDs) == 0 && data.SeatID != "" {
		data.SeatIDs = []string{data.SeatID}
	}
	if data.SeatID == "" && len(data.SeatIDs) > 0 {
		data.SeatID = data.SeatIDs[0]
	}
	return Booking{
		ID:        data.ID,
		MovieID:   data.MovieID,
		SeatID:    data.SeatID,
		SeatIDs:   data.SeatIDs,
		UserID:    data.UserID,
		Status:    data.Status,
		ExpiresAt: data.ExpiresAt,
	}, nil
}

// Confirm converts a held session into a permanent booking.
// Removes the TTL (PERSIST) so the key never expires.
func (s *RedisStore) Confirm(ctx context.Context, sessionID string, userID string) (Booking, error) {
	session, _, err := s.getSession(ctx, sessionID, userID)
	if err != nil {
		return Booking{}, err
	}
	if userID != "" && session.UserID != userID {
		return Booking{}, ErrInvalidSeatRequest
	}

	keys := buildSeatKeys(session.MovieID, session.SeatIDs)
	seatPayloads := buildSeatPayloads(session, "confirmed")
	session.Status = "confirmed"
	result, err := s.runConfirmMany(ctx, sessionID, session, keys, seatPayloads)
	if err != nil {
		return Booking{}, err
	}
	if result != 1 {
		return Booking{}, ErrSeatAlreadyBooked
	}

	for _, seatID := range session.SeatIDs {
		s.rdb.Persist(ctx, seatKey(session.MovieID, seatID))
	}
	s.rdb.Persist(ctx, sessionKey(sessionID))

	return session, nil
}

func (s *RedisStore) runConfirmMany(ctx context.Context, sessionID string, session Booking, seatKeys []string, seatPayloads []string) (int, error) {
	keys := make([]string, 0, len(seatKeys)+1)
	keys = append(keys, seatKeys...)
	keys = append(keys, sessionKey(sessionID))

	args := []interface{}{marshalBookingPayload(session)}
	for _, payload := range seatPayloads {
		args = append(args, payload)
	}

	return confirmManySeatsScript.Run(ctx, s.rdb, keys, args...).Int()
}

func (s *RedisStore) getSession(ctx context.Context, sessionID string, userID string) (Booking, string, error) {
	val, err := s.rdb.Get(ctx, sessionKey(sessionID)).Result()
	if err != nil {
		return Booking{}, "", err
	}

	session, err := parseSession(val)
	if err != nil {
		return Booking{}, "", err
	}
	if userID != "" && session.UserID != userID {
		return Booking{}, "", ErrInvalidSeatRequest
	}
	if len(session.SeatIDs) == 0 {
		return Booking{}, "", ErrInvalidSeatRequest
	}

	return session, sessionKey(sessionID), nil
}

func (s *RedisStore) Release(ctx context.Context, sessionID string, userID string) error {
	session, _, err := s.getSession(ctx, sessionID, userID)
	if err != nil {
		return err
	}

	keys := make([]string, 0, len(session.SeatIDs)+1)
	for _, seatID := range session.SeatIDs {
		keys = append(keys, seatKey(session.MovieID, seatID))
	}
	keys = append(keys, sessionKey(sessionID))
	s.rdb.Del(ctx, keys...)
	return nil
}
