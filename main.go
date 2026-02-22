package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
)

/*
Minimal SFU based on ion-sfu architecture:

1. Each client has TWO peer connections:
   - Publisher: client sends media TO server (client offers, server answers)
   - Subscriber: server sends media TO client (server offers, client answers)

2. Track flow:
   - Client publishes → Publisher.OnTrack fires → Router forwards to Subscribers
   - AddDownTrack adds track to Subscriber → triggers renegotiation
   - Subscriber creates offer → client answers

This separation avoids the transceiver direction confusion we had before.
*/

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Client represents a connected client with both publisher and subscriber
type Client struct {
	ID         string
	Publisher  *webrtc.PeerConnection
	Subscriber *webrtc.PeerConnection
	ws         *websocket.Conn
	mu         sync.Mutex
}

// SFU holds all clients and routes tracks between them
type SFU struct {
	clients    map[string]*Client
	tracks     map[string]map[string]*webrtc.TrackLocalStaticRTP // clientID -> trackID -> track
	mu         sync.RWMutex
	iceServers []webrtc.ICEServer
	publicIP   string
}

func NewSFU() *SFU {
	// STUN servers for NAT traversal
	iceServers := []webrtc.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
	}
	
	// OpenRelay TURN server - supports TURNS (TLS) on port 443
	// This works through firewalls and proxies that only allow HTTPS
	iceServers = append(iceServers, webrtc.ICEServer{
		URLs: []string{
			"turns:openrelay.metered.ca:443?transport=tcp",  // TURN over TLS (port 443)
			"turn:openrelay.metered.ca:443?transport=tcp",   // TURN over TCP (port 443)
			"turn:openrelay.metered.ca:80?transport=tcp",    // TURN over TCP (port 80)
		},
		Username:   "openrelayproject",
		Credential: "openrelayproject",
	})
	log.Printf("🧊 Using OpenRelay TURNS (TLS/443) for firewall traversal")
	
	log.Printf("🧊 ICE servers: %d configured", len(iceServers))
	
	return &SFU{
		clients:    make(map[string]*Client),
		tracks:     make(map[string]map[string]*webrtc.TrackLocalStaticRTP),
		iceServers: iceServers,
		publicIP:   os.Getenv("PUBLIC_IP"),
	}
}

func main() {
	sfu := NewSFU()

	http.HandleFunc("/ws", sfu.handleWebSocket)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "index.html")
	})

	log.Println("🚀 SFU server starting on :8080")
	
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

func (s *SFU) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer ws.Close()

	clientID := uuid.New().String()[:8]
	client := &Client{
		ID:  clientID,
		ws:  ws,
	}

	s.mu.Lock()
	s.clients[clientID] = client
	s.mu.Unlock()

	log.Printf("👋 Client %s connected", clientID)

	// Send client their ID
	ws.WriteJSON(map[string]string{"type": "welcome", "id": clientID})

	// Handle messages
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			log.Printf("❌ Client %s read error: %v", clientID, err)
			break
		}

		var data map[string]interface{}
		if err := json.Unmarshal(msg, &data); err != nil {
			log.Printf("❌ Client %s JSON error: %v", clientID, err)
			continue
		}

		msgType, _ := data["type"].(string)
		switch msgType {
		case "publish":
			// Client wants to publish (send media)
			sdp, _ := data["sdp"].(string)
			s.handlePublish(client, sdp)
		case "subscribe":
			// Client wants to subscribe (receive media)
			s.handleSubscribe(client)
		case "answer":
			// Client answered our subscriber offer
			sdp, _ := data["sdp"].(string)
			s.handleAnswer(client, sdp)
		case "ice":
			// ICE candidate
			target, _ := data["target"].(string) // "publish" or "subscribe"
			candidate := data["candidate"]
			s.handleICE(client, target, candidate)
		}
	}

	// Cleanup
	s.mu.Lock()
	delete(s.clients, clientID)
	delete(s.tracks, clientID)
	s.mu.Unlock()

	if client.Publisher != nil {
		client.Publisher.Close()
	}
	if client.Subscriber != nil {
		client.Subscriber.Close()
	}

	log.Printf("👋 Client %s disconnected", clientID)
}

// handlePublish handles client's publisher offer
func (s *SFU) handlePublish(client *Client, sdp string) {
	client.mu.Lock()
	defer client.mu.Unlock()

	if client.Publisher != nil {
		client.Publisher.Close()
	}

	// Configure ICE with public IP for NAT traversal
	var settings webrtc.SettingEngine
	
	// Use the Render hostname as a host candidate
	// This allows clients to connect via the public URL
	settings.SetNAT1To1IPs([]string{"sfu-test-2.onrender.com"}, webrtc.ICECandidateTypeHost)
	log.Printf("🌐 [%s] Publisher using hostname: sfu-test-2.onrender.com", client.ID)
	
	// Create publisher PC with settings
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settings))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: s.iceServers,
	})
	if err != nil {
		log.Printf("❌ [%s] Failed to create publisher: %v", client.ID, err)
		return
	}
	client.Publisher = pc

	// Handle incoming tracks
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Printf("🎥 [%s] Received %s track", client.ID, track.Kind())

		// Create local track for forwarding
		localTrack, err := webrtc.NewTrackLocalStaticRTP(
			track.Codec().RTPCodecCapability,
			"track-"+client.ID+"-"+string(track.Kind()),
			"stream-"+client.ID,
		)
		if err != nil {
			log.Printf("❌ [%s] Failed to create local track: %v", client.ID, err)
			return
		}

		// Store track
		s.mu.Lock()
		if s.tracks[client.ID] == nil {
			s.tracks[client.ID] = make(map[string]*webrtc.TrackLocalStaticRTP)
		}
		trackKey := string(track.Kind())
		s.tracks[client.ID][trackKey] = localTrack
		s.mu.Unlock()

		log.Printf("📦 [%s] Stored %s track, broadcasting to %d clients", client.ID, track.Kind(), len(s.clients)-1)

		// Add track to all other clients' subscribers
		s.mu.RLock()
		for otherID, otherClient := range s.clients {
			if otherID == client.ID {
				continue
			}
			otherClient.mu.Lock()
			if otherClient.Subscriber != nil {
				_, err := otherClient.Subscriber.AddTrack(localTrack)
				if err != nil {
					log.Printf("❌ [%s] Failed to add track from %s: %v", otherID, client.ID, err)
				} else {
					log.Printf("✅ [%s] Added %s track from %s", otherID, track.Kind(), client.ID)
				}
			}
			otherClient.mu.Unlock()
		}
		s.mu.RUnlock()

		// Forward RTP packets
		buf := make([]byte, 1500)
		for {
			n, _, err := track.Read(buf)
			if err != nil {
				log.Printf("📭 [%s] Track ended: %v", client.ID, err)
				return
			}
			localTrack.Write(buf[:n])
		}
	})

	// Disable trickle ICE - we'll gather all candidates before sending

	// Set remote description
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}); err != nil {
		log.Printf("❌ [%s] Failed to set remote description: %v", client.ID, err)
		return
	}

	// Create answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		log.Printf("❌ [%s] Failed to create answer: %v", client.ID, err)
		return
	}

	// Set local description and wait for ICE gathering to complete
	gatheringComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		log.Printf("❌ [%s] Failed to set local description: %v", client.ID, err)
		return
	}

	// Wait for ICE gathering to complete
	<-gatheringComplete
	log.Printf("✅ [%s] Publisher ICE gathering complete", client.ID)

	// Send answer with candidates included
	client.ws.WriteJSON(map[string]interface{}{
		"type": "publish_answer",
		"sdp":  pc.LocalDescription().SDP,
	})

	log.Printf("✅ [%s] Publisher established", client.ID)
}

// handleSubscribe creates subscriber PC and sends offer with existing tracks
func (s *SFU) handleSubscribe(client *Client) {
	client.mu.Lock()
	defer client.mu.Unlock()

	if client.Subscriber != nil {
		client.Subscriber.Close()
	}

	// Configure ICE with public hostname
	var settings webrtc.SettingEngine
	settings.SetNAT1To1IPs([]string{"sfu-test-2.onrender.com"}, webrtc.ICECandidateTypeHost)
	log.Printf("🌐 [%s] Subscriber using hostname: sfu-test-2.onrender.com", client.ID)
	
	// Create subscriber PC with settings
	api := webrtc.NewAPI(webrtc.WithSettingEngine(settings))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: s.iceServers,
	})
	if err != nil {
		log.Printf("❌ [%s] Failed to create subscriber: %v", client.ID, err)
		return
	}
	client.Subscriber = pc

	// Disable trickle ICE - we'll gather all candidates before sending

	// Add all existing tracks from other clients
	s.mu.RLock()
	trackCount := 0
	log.Printf("🔍 [%s] Looking for tracks from %d other clients", client.ID, len(s.clients))
	for otherID, tracks := range s.tracks {
		if otherID == client.ID {
			continue
		}
		log.Printf("🔍 [%s] Client %s has %d tracks", client.ID, otherID, len(tracks))
		for trackKey, track := range tracks {
			if _, err := pc.AddTrack(track); err != nil {
				log.Printf("❌ [%s] Failed to add %s track from %s: %v", client.ID, trackKey, otherID, err)
			} else {
				log.Printf("✅ [%s] Added %s track from %s to subscriber", client.ID, trackKey, otherID)
				trackCount++
			}
		}
	}
	s.mu.RUnlock()

	if trackCount == 0 {
		log.Printf("⚠️ [%s] No tracks available from other clients - publish first!", client.ID)
	} else {
		log.Printf("📦 [%s] Subscriber has %d tracks from other clients", client.ID, trackCount)
	}

	// Create offer
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		log.Printf("❌ [%s] Failed to create subscriber offer: %v", client.ID, err)
		return
	}

	// Set local description and wait for ICE gathering to complete
	gatheringComplete := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		log.Printf("❌ [%s] Failed to set local description: %v", client.ID, err)
		return
	}

	// Wait for ICE gathering to complete
	<-gatheringComplete
	log.Printf("✅ [%s] Subscriber ICE gathering complete", client.ID)

	// Send offer with candidates included
	client.ws.WriteJSON(map[string]interface{}{
		"type": "subscribe_offer",
		"sdp":  pc.LocalDescription().SDP,
	})

	log.Printf("✅ [%s] Subscriber offer sent", client.ID)
}

// handleAnswer handles client's answer to our subscriber offer
func (s *SFU) handleAnswer(client *Client, sdp string) {
	client.mu.Lock()
	defer client.mu.Unlock()

	if client.Subscriber == nil {
		log.Printf("❌ [%s] No subscriber to set answer", client.ID)
		return
	}

	log.Printf("📝 [%s] Setting subscriber answer (%d bytes)", client.ID, len(sdp))

	if err := client.Subscriber.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}); err != nil {
		log.Printf("❌ [%s] Failed to set remote description: %v", client.ID, err)
		return
	}

	log.Printf("✅ [%s] Subscriber answer set - connection should establish", client.ID)
}

// handleICE handles ICE candidates
func (s *SFU) handleICE(client *Client, target string, candidate interface{}) {
	client.mu.Lock()
	defer client.mu.Unlock()

	var pc *webrtc.PeerConnection
	if target == "publish" {
		pc = client.Publisher
	} else {
		pc = client.Subscriber
	}

	if pc == nil {
		return
	}

	// Convert candidate to ICECandidateInit
	candidateBytes, _ := json.Marshal(candidate)
	var iceCandidate webrtc.ICECandidateInit
	if err := json.Unmarshal(candidateBytes, &iceCandidate); err != nil {
		log.Printf("❌ [%s] Failed to parse ICE candidate: %v", client.ID, err)
		return
	}

	if err := pc.AddICECandidate(iceCandidate); err != nil {
		log.Printf("❌ [%s] Failed to add ICE candidate: %v", client.ID, err)
	} else {
		log.Printf("🧊 [%s] ICE candidate added to %s", client.ID, target)
	}
}