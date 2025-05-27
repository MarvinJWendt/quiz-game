package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		// In production, you should check the origin
		return true
	},
}

type GameState struct {
	CurrentQuestion     *Question           `json:"currentQuestion,omitempty"`
	Players             map[string]*Player  `json:"players"`
	DisconnectedPlayers map[string]*Player  `json:"-"` // Store disconnected players
	Answers             map[string][]Answer `json:"answers"`
	Leaderboard         []LeaderboardEntry  `json:"leaderboard"`
	CountdownEndTime    *time.Time          `json:"countdownEndTime,omitempty"`
	Round               int                 `json:"round"`
	RoundEnded          bool                `json:"roundEnded"`
	Moderator           *websocket.Conn     `json:"-"`
	mu                  sync.RWMutex
}

type Question struct {
	Text    string    `json:"text"`
	Type    string    `json:"type"`            // "text" or "number"
	Image   string    `json:"image,omitempty"` // Base64 encoded image data
	AskedAt time.Time `json:"askedAt"`
}

type Player struct {
	Name       string          `json:"name"`
	Score      int             `json:"score"`
	IsMod      bool            `json:"isMod"`
	Connection *websocket.Conn `json:"-"`
}

type Answer struct {
	PlayerName  string          `json:"playerName"`
	Text        string          `json:"text"`
	Status      string          `json:"status"` // "pending", "accepted", "rejected"
	SubmittedAt time.Time       `json:"submittedAt"`
	JudgedAt    *time.Time      `json:"judgedAt,omitempty"` // Track when the answer was judged
	Votes       map[string]bool `json:"votes"`              // playerName -> true (upvote) or false (downvote)
	VoteScore   int             `json:"voteScore"`          // Net vote score (upvotes - downvotes)
}

type LeaderboardEntry struct {
	PlayerName string `json:"playerName"`
	Score      int    `json:"score"`
}

type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

var game = GameState{
	Players:             make(map[string]*Player),
	DisconnectedPlayers: make(map[string]*Player),
	Answers:             make(map[string][]Answer),
	Leaderboard:         make([]LeaderboardEntry, 0),
	Round:               0,
}

func main() {
	http.HandleFunc("/ws", handleWebSocket)

	// Serve static files
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFile(w, r, "index.html")
		} else if r.URL.Path == "/moderator" {
			http.ServeFile(w, r, "moderator.html")
		} else if r.URL.Path == "/test" {
			http.ServeFile(w, r, "test.html")
		} else {
			http.NotFound(w, r)
		}
	})

	log.Printf("Starting server on :8080")
	log.Printf("Player interface: http://localhost:8080/")
	log.Printf("Moderator interface: http://localhost:8080/moderator")
	log.Printf("Image test: http://localhost:8080/test")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal("ListenAndServe:", err)
	}
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	defer conn.Close()

	// Handle player registration and message routing
	handlePlayerConnection(conn)
}

func handlePlayerConnection(conn *websocket.Conn) {
	// Set ping handler
	conn.SetPingHandler(func(appData string) error {
		return conn.WriteControl(websocket.PongMessage, []byte{}, time.Now().Add(time.Second))
	})

	// Read initial registration message
	var msg Message
	if err := conn.ReadJSON(&msg); err != nil {
		if !isNormalClosure(err) {
			log.Println("Registration read error:", err)
		}
		return
	}

	if msg.Type != "register" {
		log.Println("First message must be registration")
		return
	}

	var registration struct {
		Name  string `json:"name"`
		IsMod bool   `json:"isMod"`
	}
	if err := json.Unmarshal(msg.Payload, &registration); err != nil {
		log.Println("Registration parse error:", err)
		return
	}

	game.mu.Lock()
	if registration.IsMod {
		game.Moderator = conn
		// Send current game state to moderator
		if err := conn.WriteJSON(Message{
			Type:    "game_state",
			Payload: marshalGameState(),
		}); err != nil {
			log.Println("Write error to moderator:", err)
		}
	} else {
		// Check if player is reconnecting
		var player *Player
		if disconnectedPlayer, exists := game.DisconnectedPlayers[registration.Name]; exists {
			// Reconnecting player - restore their data
			player = disconnectedPlayer
			player.Connection = conn
			delete(game.DisconnectedPlayers, registration.Name)
		} else if _, exists := game.Players[registration.Name]; exists {
			// Player trying to connect with existing name
			game.mu.Unlock()
			conn.WriteJSON(Message{
				Type:    "error",
				Payload: json.RawMessage(`{"message": "Name already taken"}`),
			})
			return
		} else {
			// New player
			player = &Player{
				Name:       registration.Name,
				Score:      0,
				IsMod:      false,
				Connection: conn,
			}
		}
		game.Players[registration.Name] = player
		updateLeaderboard()

		// Send current game state to player while holding the lock
		if err := conn.WriteJSON(Message{
			Type:    "game_state",
			Payload: marshalGameState(),
		}); err != nil {
			log.Println("Write error to player:", err)
		}
	}
	game.mu.Unlock()

	// Handle messages
	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			if !isNormalClosure(err) {
				log.Printf("Read error from %s: %v", registration.Name, err)
			}
			break
		}

		if msg.Type == "ping" {
			// Respond to ping with pong
			if err := conn.WriteJSON(Message{Type: "pong"}); err != nil {
				log.Printf("Error sending pong to %s: %v", registration.Name, err)
				break
			}
			continue
		}

		if registration.IsMod {
			handleModeratorMessage(msg)
		} else {
			handlePlayerMessage(registration.Name, msg)
		}
	}

	// Cleanup on disconnect
	game.mu.Lock()
	if registration.IsMod {
		game.Moderator = nil
	} else {
		// Store player data instead of deleting
		if player, exists := game.Players[registration.Name]; exists {
			game.DisconnectedPlayers[registration.Name] = player
			delete(game.Players, registration.Name)
			updateLeaderboard()
		}
	}
	game.mu.Unlock()
}

// isNormalClosure checks if the error is from a normal WebSocket closure
func isNormalClosure(err error) bool {
	if websocket.IsCloseError(err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseNoStatusReceived) {
		return true
	}
	return strings.Contains(err.Error(), "websocket: close 1006")
}

func handleModeratorMessage(msg Message) {
	game.mu.Lock()
	defer game.mu.Unlock()

	switch msg.Type {
	case "ask_question":
		var question Question
		if err := json.Unmarshal(msg.Payload, &question); err != nil {
			log.Println("Question parse error:", err)
			return
		}
		question.AskedAt = time.Now()
		game.CurrentQuestion = &question
		game.Round++
		game.Answers = make(map[string][]Answer)
		game.RoundEnded = false
		broadcastGameState()

	case "end_round":
		game.RoundEnded = true
		game.CountdownEndTime = nil
		broadcastGameState()

	case "judge_answer":
		var judge struct {
			PlayerName string `json:"playerName"`
			Round      int    `json:"round"`
			Accept     bool   `json:"accept"`
		}
		if err := json.Unmarshal(msg.Payload, &judge); err != nil {
			log.Println("Judge parse error:", err)
			return
		}

		// Check if target player exists
		targetPlayer, exists := game.Players[judge.PlayerName]
		if !exists {
			log.Printf("Target player %s not found", judge.PlayerName)
			return
		}

		if answers, exists := game.Answers[judge.PlayerName]; exists && len(answers) > 0 {
			lastAnswer := &answers[len(answers)-1]
			// Only judge if the answer is still pending and hasn't been judged before
			if lastAnswer.Status == "pending" && lastAnswer.JudgedAt == nil {
				now := time.Now()
				lastAnswer.JudgedAt = &now
				if judge.Accept {
					lastAnswer.Status = "accepted"
					targetPlayer.Score++
				} else {
					lastAnswer.Status = "rejected"
				}
				updateLeaderboard()
				broadcastGameState()
			}
		}

	case "start_countdown":
		var countdown struct {
			Seconds int `json:"seconds"`
		}
		if err := json.Unmarshal(msg.Payload, &countdown); err != nil {
			log.Printf("Countdown parse error: %v. Payload: %s", err, string(msg.Payload))
			return
		}
		if countdown.Seconds < 5 {
			log.Printf("Invalid countdown duration: %d seconds (minimum 5)", countdown.Seconds)
			return
		}
		endTime := time.Now().Add(time.Duration(countdown.Seconds) * time.Second)
		game.CountdownEndTime = &endTime
		broadcastGameState()

	case "reset_game":
		game.CurrentQuestion = nil
		game.Answers = make(map[string][]Answer)
		game.Round = 0
		game.CountdownEndTime = nil
		game.RoundEnded = false

		// Reset all players to unregistered state
		for _, player := range game.Players {
			if player.Connection != nil {
				player.Connection.WriteJSON(Message{
					Type:    "reset",
					Payload: json.RawMessage(`{}`),
				})
			}
		}
		game.Players = make(map[string]*Player)
		game.Leaderboard = make([]LeaderboardEntry, 0)
		broadcastGameState()

	case "update_score":
		var scoreUpdate struct {
			PlayerName string `json:"playerName"`
			Score      int    `json:"score"`
		}
		if err := json.Unmarshal(msg.Payload, &scoreUpdate); err != nil {
			log.Println("Score update parse error:", err)
			return
		}

		// Check if target player exists
		targetPlayer, exists := game.Players[scoreUpdate.PlayerName]
		if !exists {
			log.Printf("Target player %s not found for score update", scoreUpdate.PlayerName)
			return
		}

		// Validate score (must be non-negative)
		if scoreUpdate.Score < 0 {
			log.Printf("Invalid score %d for player %s", scoreUpdate.Score, scoreUpdate.PlayerName)
			return
		}

		// Update the player's score
		targetPlayer.Score = scoreUpdate.Score
		updateLeaderboard()
		broadcastGameState()
	}
}

func handlePlayerMessage(playerName string, msg Message) {
	game.mu.Lock()
	defer game.mu.Unlock()

	_, exists := game.Players[playerName]
	if !exists {
		log.Printf("Player %s not found", playerName)
		return
	}

	switch msg.Type {
	case "submit_answer":
		if game.CurrentQuestion == nil || game.RoundEnded {
			return
		}
		var answer struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Payload, &answer); err != nil {
			log.Println("Answer parse error:", err)
			return
		}

		newAnswer := Answer{
			PlayerName:  playerName,
			Text:        answer.Text,
			Status:      "pending",
			SubmittedAt: time.Now(),
			Votes:       make(map[string]bool),
			VoteScore:   0,
		}

		game.Answers[playerName] = append(game.Answers[playerName], newAnswer)
		broadcastGameState()

	case "vote_answer":
		if !game.RoundEnded {
			return // Can only vote after round ends
		}

		var vote struct {
			TargetPlayerName string `json:"targetPlayerName"`
			AnswerIndex      int    `json:"answerIndex"`
			IsUpvote         bool   `json:"isUpvote"`
		}
		if err := json.Unmarshal(msg.Payload, &vote); err != nil {
			log.Println("Vote parse error:", err)
			return
		}

		// Check if target player and answer exist
		answers, exists := game.Answers[vote.TargetPlayerName]
		if !exists || vote.AnswerIndex < 0 || vote.AnswerIndex >= len(answers) {
			log.Printf("Invalid vote target: player %s, answer index %d", vote.TargetPlayerName, vote.AnswerIndex)
			return
		}

		answer := &answers[vote.AnswerIndex]

		// Remove previous vote if exists
		if previousVote, hasVoted := answer.Votes[playerName]; hasVoted {
			if previousVote {
				answer.VoteScore-- // Remove previous upvote
			} else {
				answer.VoteScore++ // Remove previous downvote
			}
		}

		// Add new vote
		answer.Votes[playerName] = vote.IsUpvote
		if vote.IsUpvote {
			answer.VoteScore++
		} else {
			answer.VoteScore--
		}

		broadcastGameState()
	}
}

func updateLeaderboard() {
	game.Leaderboard = make([]LeaderboardEntry, 0, len(game.Players))
	for _, player := range game.Players {
		game.Leaderboard = append(game.Leaderboard, LeaderboardEntry{
			PlayerName: player.Name,
			Score:      player.Score,
		})
	}

	// Sort leaderboard by score in descending order (highest score first)
	sort.Slice(game.Leaderboard, func(i, j int) bool {
		return game.Leaderboard[i].Score > game.Leaderboard[j].Score
	})
}

func broadcastGameState() {
	payload := marshalGameState()

	// Send to moderator
	if game.Moderator != nil {
		if err := game.Moderator.WriteJSON(Message{
			Type:    "game_state",
			Payload: payload,
		}); err != nil {
			log.Println("Broadcast error to moderator:", err)
		}
	}

	// Send to players
	for _, player := range game.Players {
		if err := player.Connection.WriteJSON(Message{
			Type:    "game_state",
			Payload: payload,
		}); err != nil {
			log.Println("Broadcast error:", err)
		}
	}
}

func marshalGameState() json.RawMessage {
	state := struct {
		CurrentQuestion  *Question           `json:"currentQuestion,omitempty"`
		Players          map[string]*Player  `json:"players"`
		Answers          map[string][]Answer `json:"answers"`
		Leaderboard      []LeaderboardEntry  `json:"leaderboard"`
		CountdownEndTime *time.Time          `json:"countdownEndTime,omitempty"`
		Round            int                 `json:"round"`
		RoundEnded       bool                `json:"roundEnded"`
	}{
		CurrentQuestion:  game.CurrentQuestion,
		Players:          game.Players,
		Answers:          game.Answers,
		Leaderboard:      game.Leaderboard,
		CountdownEndTime: game.CountdownEndTime,
		Round:            game.Round,
		RoundEnded:       game.RoundEnded,
	}

	payload, _ := json.Marshal(state)
	return payload
}
