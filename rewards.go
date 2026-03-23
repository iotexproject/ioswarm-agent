package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

type rewardEntry struct {
	Epoch     uint64  `json:"epoch"`
	AmountIOTX float64 `json:"amount_iotx"`
	Tasks     uint64  `json:"tasks"`
	Accuracy  float64 `json:"accuracy_pct"`
	Rank      int     `json:"rank"`
	Bonus     bool    `json:"bonus_applied"`
}

type rewardsResponse struct {
	AgentID string        `json:"agent_id"`
	History []rewardEntry `json:"history"`
	Totals  struct {
		EarnedIOTX  float64 `json:"earned_iotx"`
		TotalTasks  uint64  `json:"total_tasks"`
		AvgAccuracy float64 `json:"avg_accuracy"`
	} `json:"totals"`
}

func runRewards(args []string) {
	fs := flag.NewFlagSet("rewards", flag.ExitOnError)
	coordURL := fs.String("coordinator", "", "coordinator HTTP API URL (e.g., http://delegate.example.com:14690)")
	agentID := fs.String("agent-id", "", "agent ID to query")
	fs.Parse(args)

	if *coordURL == "" {
		*coordURL = os.Getenv("IOSWARM_COORDINATOR_HTTP")
	}
	if *agentID == "" {
		*agentID = os.Getenv("IOSWARM_AGENT_ID")
	}

	if *coordURL == "" || *agentID == "" {
		fmt.Fprintf(os.Stderr, "Usage: ioswarm-agent rewards --coordinator=http://host:14690 --agent-id=<id>\n")
		os.Exit(1)
	}

	url := fmt.Sprintf("%s/api/rewards?agent=%s", *coordURL, *agentID)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "error: HTTP %d from coordinator\n", resp.StatusCode)
		os.Exit(1)
	}

	var data rewardsResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		fmt.Fprintf(os.Stderr, "error parsing response: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Agent: %s\n\n", data.AgentID)

	if len(data.History) > 0 {
		fmt.Println("Reward History:")
		fmt.Printf("  %-8s %-8s %-10s %-10s %-6s %s\n", "Epoch", "Tasks", "Accuracy", "IOTX", "Rank", "Bonus")
		for _, e := range data.History {
			bonus := ""
			if e.Bonus {
				bonus = "+20%"
			}
			fmt.Printf("  %-8d %-8d %-10.1f%% %-10.4f %-6d %s\n",
				e.Epoch, e.Tasks, e.Accuracy, e.AmountIOTX, e.Rank, bonus)
		}
		fmt.Println()
	} else {
		fmt.Println("No reward history yet.")
	}

	fmt.Printf("Totals: %.4f IOTX earned | %d tasks | %.1f%% avg accuracy\n",
		data.Totals.EarnedIOTX, data.Totals.TotalTasks, data.Totals.AvgAccuracy)
}
