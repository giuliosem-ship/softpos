# SoftPOS México — Cliente Nativo iOS (Swift + ProximityReader)

Directorio reservado para el cliente nativo iOS (Fase 3 del Roadmap), utilizando:
- **SwiftUI**
- **Apple Tap to Pay on iPhone (`ProximityReader` framework)** con el entitlement `com.apple.developer.proximity-reader.payment.acceptance`
- **Apple App Attest (`DeviceCheck`)** para atestación criptográfica del dispositivo ante el backend en Go (`pkg/api`).
