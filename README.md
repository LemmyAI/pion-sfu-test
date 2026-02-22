# Pion SFU Test

A minimal WebRTC Selective Forwarding Unit (SFU) using Pion.

## ⚠️ Deployment Limitation

**Render.com does NOT support WebRTC** - they block incoming UDP traffic which WebRTC requires for media transmission. The SFU will not work on Render's free tier.

### Working Deployment Options:

1. **Fly.io** - Supports UDP, good for WebRTC
   ```bash
   fly launch
   fly deploy
   ```

2. **Railway** - Supports UDP traffic

3. **Your own VPS** - Any cloud VM with public IP (DigitalOcean, Linode, etc.)

4. **LiveKit Cloud** - Managed WebRTC service (free tier available)
   - https://cloud.livekit.io

## Local Development

```bash
go run main.go
# Open http://localhost:8080 in two tabs
```

## What Works

✅ Local development (both tabs on same machine)
✅ Signaling via WebSocket
✅ SDP exchange
✅ ICE gathering (STUN/TURN)

## What Doesn't Work on Render

❌ ICE connectivity - UDP traffic is blocked
❌ Media transmission - No direct peer connection possible

## Architecture

```
Client 1 (Publisher) → WebSocket → SFU Server → WebSocket → Client 2 (Subscriber)
                    ↘ UDP/RTP ↗              ↘ UDP/RTP ↗
                      (BLOCKED on Render)
```

The signaling works fine (WebSocket), but media requires UDP which Render blocks.