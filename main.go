package main

import (
	"encoding/json"
	"log"
	"net/http"
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
	CurrentQuestion  *Question           `json:"currentQuestion,omitempty"`
	Players          map[string]*Player  `json:"players"`
	Answers          map[string][]Answer `json:"answers"`
	Leaderboard      []LeaderboardEntry  `json:"leaderboard"`
	CountdownEndTime *time.Time          `json:"countdownEndTime,omitempty"`
	Round            int                 `json:"round"`
	RoundEnded       bool                `json:"roundEnded"`
	Moderator        *websocket.Conn     `json:"-"`
	mu               sync.RWMutex
}

type Question struct {
	Text    string    `json:"text"`
	Type    string    `json:"type"` // "text" or "number"
	AskedAt time.Time `json:"askedAt"`
}

type Player struct {
	Name       string          `json:"name"`
	Score      int             `json:"score"`
	IsMod      bool            `json:"isMod"`
	Connection *websocket.Conn `json:"-"`
}

type Answer struct {
	PlayerName  string    `json:"playerName"`
	Text        string    `json:"text"`
	Status      string    `json:"status"` // "pending", "accepted", "rejected"
	SubmittedAt time.Time `json:"submittedAt"`
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
	Players:     make(map[string]*Player),
	Answers:     make(map[string][]Answer),
	Leaderboard: make([]LeaderboardEntry, 0),
	Round:       0,
}

func main() {
	http.HandleFunc("/ws", handleWebSocket)

	log.Printf("Starting server on :8080")
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
	// Read initial registration message
	var msg Message
	if err := conn.ReadJSON(&msg); err != nil {
		log.Println("Read error:", err)
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
		// If there's already a moderator, reject the connection
		if game.Moderator != nil {
			game.mu.Unlock()
			conn.WriteJSON(Message{
				Type:    "error",
				Payload: json.RawMessage(`{"message": "A moderator is already connected"}`),
			})
			conn.Close()
			return
		}
		game.Moderator = conn
	} else {
		game.Players[registration.Name] = &Player{
			Name:       registration.Name,
			Score:      0,
			IsMod:      false,
			Connection: conn,
		}
		updateLeaderboard()
	}
	game.mu.Unlock()

	// Send current game state
	sendGameState(conn)

	// Handle messages
	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			log.Println("Read error:", err)
			break
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
		delete(game.Players, registration.Name)
		updateLeaderboard()
	}
	game.mu.Unlock()
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
			if judge.Accept {
				lastAnswer.Status = "accepted"
				targetPlayer.Score++
			} else {
				lastAnswer.Status = "rejected"
			}
			updateLeaderboard()
			broadcastGameState()
		}

	case "start_countdown":
		var countdown struct {
			Seconds int `json:"seconds"`
		}
		if err := json.Unmarshal(msg.Payload, &countdown); err != nil {
			log.Println("Countdown parse error:", err)
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
		}

		game.Answers[playerName] = append(game.Answers[playerName], newAnswer)
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
}

func sendGameState(conn *websocket.Conn) {
	game.mu.RLock()
	defer game.mu.RUnlock()

	if err := conn.WriteJSON(Message{
		Type:    "game_state",
		Payload: marshalGameState(),
	}); err != nil {
		log.Println("Write error:", err)
	}
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
