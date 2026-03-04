package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

const hevyAPIBase = "https://api.hevyapp.com"

func main() {
	if os.Getenv("GO_ENV") == "development" {
		_ = godotenv.Load(".env.development")
	}
	for _, key := range []string{"HEVY_API_KEY", "DISCORD_WEBHOOK_URL"} {
		if os.Getenv(key) == "" {
			log.Fatalf("%s must be set", key)
		}
	}
	http.HandleFunc("/", healthHandler)
	http.HandleFunc("/ingest", ingestHandler)
	log.Println("Server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

var hevyClient = &http.Client{Timeout: 4 * time.Second}

func hevyGet(apiKey, path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, hevyAPIBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("api-key", apiKey)
	resp, err := hevyClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &hevyAPIError{StatusCode: resp.StatusCode, Body: body}
	}
	return body, nil
}

type hevyWebhookPayload struct {
	WorkoutID string `json:"workoutId"`
}

type hevyWorkout struct {
	ID          string        `json:"id"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	StartTime   string        `json:"start_time"`
	EndTime     string        `json:"end_time"`
	Exercises   []hevyExercise `json:"exercises"`
}

type hevyExercise struct {
	Title string    `json:"title"`
	Sets  []hevySet `json:"sets"`
}

type hevySet struct {
	Type             string   `json:"type"`
	WeightKg         *float64 `json:"weight_kg"`
	Reps             *int     `json:"reps"`
	DistanceMeters   *float64 `json:"distance_meters"`
	DurationSeconds  *int     `json:"duration_seconds"`
	RPE              *float64 `json:"rpe"`
}

type hevyAPIError struct {
	StatusCode int
	Body       []byte
}

func (e *hevyAPIError) Error() string { return string(e.Body) }

// Discord embed types
type discordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color"`
	Fields      []discordField `json:"fields"`
	Footer      *discordFooter `json:"footer,omitempty"`
	Timestamp   string         `json:"timestamp,omitempty"`
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type discordFooter struct {
	Text string `json:"text"`
}

const discordColor = 0x2ecc71

func buildWorkoutEmbed(w hevyWorkout) discordEmbed {
	title := w.Title
	if title == "" {
		title = "Workout logged"
	}
	fields := []discordField{
		{Name: "Duration", Value: formatDuration(w.StartTime, w.EndTime), Inline: true},
	}
	for _, ex := range w.Exercises {
		value := formatSets(ex.Sets)
		if value == "" {
			value = "—"
		}
		fields = append(fields, discordField{Name: ex.Title, Value: value, Inline: false})
	}
	return discordEmbed{
		Title:       title,
		Description: strings.SplitN(w.Description, "\n", 2)[0],
		Color:       discordColor,
		Fields:      fields,
		Footer:      &discordFooter{Text: fmt.Sprintf("Hevy • hevy-workout-id: %s", w.ID)},
		Timestamp:   w.EndTime,
	}
}

func formatDuration(start, end string) string {
	if start == "" || end == "" {
		return "—"
	}
	t1, err1 := time.Parse(time.RFC3339, start)
	t2, err2 := time.Parse(time.RFC3339, end)
	if err1 != nil || err2 != nil {
		return "—"
	}
	d := t2.Sub(t1)
	if d < time.Minute {
		return "< 1 min"
	}
	return fmt.Sprintf("%.0f min", d.Minutes())
}

const kgToLbs = 2.2046226218

func formatSets(sets []hevySet) string {
	var lines []string
	for _, s := range sets {
		switch {
		case s.WeightKg != nil && s.Reps != nil:
			lines = append(lines, fmt.Sprintf("%.0f lbs × %d", math.Round(*s.WeightKg*kgToLbs), *s.Reps))
		case s.Reps != nil:
			lines = append(lines, fmt.Sprintf("%d reps", *s.Reps))
		case s.DistanceMeters != nil:
			lines = append(lines, fmt.Sprintf("%.0f m", *s.DistanceMeters))
		case s.DurationSeconds != nil:
			lines = append(lines, fmt.Sprintf("%d s", *s.DurationSeconds))
		default:
			lines = append(lines, "—")
		}
	}
	return strings.Join(lines, "\n")
}

func sendDiscordWebhook(webhookURL string, embed discordEmbed) error {
	body, _ := json.Marshal(struct {
		Embeds []discordEmbed `json:"embeds"`
	}{Embeds: []discordEmbed{embed}})
	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discord webhook %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func ingestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload hevyWebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	body, err := hevyGet(os.Getenv("HEVY_API_KEY"), "/v1/workouts/"+payload.WorkoutID)
	if err != nil {
		log.Printf("Fetch workout %s: %v", payload.WorkoutID, err)
		w.WriteHeader(http.StatusOK)
		return
	}

	var workout hevyWorkout
	if err := json.Unmarshal(body, &workout); err != nil {
		log.Printf("Parse workout %s: %v", payload.WorkoutID, err)
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := sendDiscordWebhook(os.Getenv("DISCORD_WEBHOOK_URL"), buildWorkoutEmbed(workout)); err != nil {
		log.Printf("Discord webhook: %v", err)
	}
	w.WriteHeader(http.StatusOK)
}
