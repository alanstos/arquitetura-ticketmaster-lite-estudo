// loadtest dispara N reservas simultâneas para o MESMO assento e mostra
// que apenas uma é aceita (201) e as demais recebem 409.
//
// Uso (com o docker compose de pé):
//
//	docker compose exec backend /app/loadtest -n 50 -event jota-quest-rio-de-janeiro
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
)

type seat struct {
	TicketID int64  `json:"ticket_id"`
	Label    string `json:"label"`
	Status   string `json:"status"`
}

type event struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Seats []seat `json:"seats"`
}

func main() {
	api := flag.String("api", "http://localhost:8080", "URL base da API")
	slug := flag.String("event", "jota-quest-rio-de-janeiro", "slug do evento")
	n := flag.Int("n", 50, "número de requisições concorrentes")
	flag.Parse()

	resp, err := http.Get(*api + "/api/events/" + *slug)
	if err != nil {
		fail(err)
	}
	var ev event
	if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
		fail(err)
	}
	resp.Body.Close()

	var target *seat
	for i := range ev.Seats {
		if ev.Seats[i].Status == "AVAILABLE" {
			target = &ev.Seats[i]
			break
		}
	}
	if target == nil {
		fail(fmt.Errorf("nenhum assento livre em %s", *slug))
	}
	fmt.Printf("Evento %q — disparando %d reservas simultâneas para o assento %s (ticket %d)\n",
		ev.Name, *n, target.Label, target.TicketID)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[int]int{}

	for i := 0; i < *n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]any{
				"event_id":       ev.ID,
				"ticket_ids":     []int64{target.TicketID},
				"customer_name":  fmt.Sprintf("Cliente %d", i),
				"customer_email": fmt.Sprintf("cliente%d@example.com", i),
			})
			<-start // todas as goroutines partem juntas
			r, err := http.Post(*api+"/api/reserve", "application/json", bytes.NewReader(body))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				counts[-1]++
				return
			}
			r.Body.Close()
			counts[r.StatusCode]++
		}(i)
	}
	close(start)
	wg.Wait()

	fmt.Println("Resultado por status HTTP:")
	for code, c := range counts {
		label := map[int]string{201: "reservado", 409: "conflito (assento já ocupado)", -1: "erro de rede"}[code]
		fmt.Printf("  %d %-32s %d\n", code, label, c)
	}
	if counts[201] == 1 {
		fmt.Println("OK: exatamente uma reserva aceita.")
	} else {
		fmt.Printf("FALHA: esperado 1 reserva aceita, obtido %d\n", counts[201])
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "erro:", err)
	os.Exit(1)
}
