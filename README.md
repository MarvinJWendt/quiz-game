# Quiz Game

A real-time quiz game application where a moderator can host quiz sessions and participants can join to answer questions.

## Features

### Moderator Features
- Ask questions (text or number based)
- Start countdown timer for answers
- Review and accept/decline participant answers
- View leaderboard
- Reset game
- Manage game rounds

### Participant Features
- View current question
- Submit answers
- View leaderboard
- See all participants' answers and their status
- Real-time countdown timer

## Technical Stack

### Frontend
- HTML5
- CSS3
- JavaScript
- WebSocket for real-time communication
- Libraries (via CDN):
  - Tailwind CSS for styling
  - Alpine.js for reactivity

### Backend
- Go
- Gorilla WebSocket for WebSocket handling
- Standard library for HTTP server

## URLs
- Frontend: https://quiz.mjw.dev
- Backend API: https://quiz-api.mjw.dev
- Moderator Interface: https://quiz.mjw.dev/moderator

## Development

### Backend
1. Ensure Go is installed
2. Run the backend:
   ```bash
   go run main.go
   ```

### Frontend
The frontend is static HTML/JS/CSS and can be served by any web server. 