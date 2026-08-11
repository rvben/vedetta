package main

import (
	"context"

	"go.opentelemetry.io/otel/trace"

	"github.com/rvben/vedetta/internal/api"
	"github.com/rvben/vedetta/internal/config"
	eventprocessor "github.com/rvben/vedetta/internal/event"
	"github.com/rvben/vedetta/internal/storage"
)

func runEventLoop(ctx context.Context, cfg *config.Config, db *storage.DB, sub *subsystems, server *api.Server, tracer trace.Tracer) {
	var runtimeServer eventprocessor.RuntimeServer
	if server != nil {
		runtimeServer = server
	}
	var recorder eventprocessor.Recorder
	if sub.recorder != nil {
		recorder = sub.recorder
	}
	var cameras eventprocessor.CameraLookup
	if sub.manager != nil {
		cameras = sub.manager
	}
	var embedder eventprocessor.ObjectEmbedder
	if sub.objectEmbedder != nil {
		embedder = sub.objectEmbedder
	}
	var faceRecognizer eventprocessor.FaceRecognizer
	if sub.faceRecognizer != nil {
		faceRecognizer = sub.faceRecognizer
	}

	processor, err := eventprocessor.NewProcessor(eventprocessor.Options{
		Config:   cfg,
		DB:       db,
		Recorder: recorder,
		Publisher: func() eventprocessor.Publisher {
			client := sub.mqttClient.Load()
			if client == nil {
				return nil
			}
			return client
		},
		Notifier:       sub.notifier,
		Server:         runtimeServer,
		Cameras:        cameras,
		ObjectEmbedder: embedder,
		FaceRecognizer: faceRecognizer,
		Inputs: eventprocessor.Inputs{
			Events:         sub.events,
			EventEnds:      sub.eventEnds,
			PresenceEvents: sub.presenceEvents,
			FaceEvents:     sub.faceEvents,
			MotionActivity: sub.motionActivity,
			Detections:     sub.detections,
		},
		Tracer: tracer,
	})
	if err != nil {
		panic(err)
	}
	go processor.Run(ctx)
}
