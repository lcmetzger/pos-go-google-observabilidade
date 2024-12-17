package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type ServiceRequest struct {
	Cep string `json:"cep"`
}

type ServiceBResponse struct {
	City  string  `json:"city"`
	TempC float64 `json:"temp_C"`
	TempF float64 `json:"temp_F"`
	TempK float64 `json:"temp_K"`
}

type ViaCEPResponse struct {
	Localidade string `json:"localidade"`
}

type ServiceResponse struct {
	City  string  `json:"city"`
	TempC float64 `json:"temp_C"`
	TempF float64 `json:"temp_F"`
	TempK float64 `json:"temp_K"`
}

var tracer = otel.Tracer("Tracer-Serviços")

func main() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	shutdown, err := initProvider("Service A", "otel-collector:4317")
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := shutdown(ctx); err != nil {
			log.Fatal("failed to shutdown TraceProvider: %w", err)
		}
	}()

	go func() {
		log.Println("Starting server on port 3333")
		http.HandleFunc("POST /cep", cepHandler)
		log.Fatal(http.ListenAndServe(":3333", nil))
	}()

	select {
	case <-sigCh:
		log.Println("Shutting down gracefuly, Ctrl+C pressed")
	case <-ctx.Done():
		log.Println("Shutting down due other reason...")
	}

	_, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
}

func cepHandler(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "Iniciando busca de CEP")
	defer span.End()

	var serviceReq ServiceRequest
	err := json.NewDecoder(r.Body).Decode(&serviceReq)
	if len(serviceReq.Cep) != 8 {
		http.Error(w, "invalid zipcode", http.StatusUnprocessableEntity)
		return
	}

	city, err := getCityByCEP(serviceReq.Cep)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// Chama o serviço B
	ctx, spanB := tracer.Start(ctx, "Invocando o Serviço B para buscar temperatura")
	defer spanB.End()

	var serviceBRespose ServiceBResponse

	var req *http.Request
	req, err = http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://service_b:4444/?cep=%s", serviceReq.Cep), nil)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	defer resp.Body.Close()
	err = json.NewDecoder(resp.Body).Decode(&serviceBRespose)

	response := &ServiceResponse{
		City:  city,
		TempC: serviceBRespose.TempC,
		TempF: serviceBRespose.TempF,
		TempK: serviceBRespose.TempK,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func getCityByCEP(cep string) (string, error) {
	resp, err := http.Get(fmt.Sprintf("http://viacep.com.br/ws/%s/json/", cep))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("can not find zipcode")
	}

	var viaCEP ViaCEPResponse
	if err := json.NewDecoder(resp.Body).Decode(&viaCEP); err != nil {
		return "", err
	}

	if viaCEP.Localidade == "" {
		return "", fmt.Errorf("can not find zipcode")
	}

	return viaCEP.Localidade, nil
}

func initProvider(serviceName, collectorURL string) (func(context.Context) error, error) {
	ctx := context.Background()

	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(serviceName)))
	if err != nil {
		return nil, fmt.Errorf("failed do create resource %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	conn, err := grpc.NewClient(collectorURL,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create gRPC connection: %w", err)
	}

	traceExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithGRPCConn(conn))
	if err != nil {
		return nil, fmt.Errorf("failed to create trace exporter: %w", err)
	}

	bsp := sdktrace.NewBatchSpanProcessor(traceExporter)
	traceProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
	)
	otel.SetTracerProvider(traceProvider)
	otel.SetTextMapPropagator((propagation.TraceContext{}))

	return traceProvider.Shutdown, nil

}
