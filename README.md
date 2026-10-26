# EVE Produktionsrechner

Desktop-Tool für Windows und macOS, das den Gewinn pro Item in der EVE-Online-Produktion berechnet: Verkaufspreis minus Material, Jobkosten (Systemindex, Facility-Steuer, SCC-Zuschlag) sowie Verkaufssteuer und Maklergebühr.

- Blaupausen-Rezepte und Itemnamen aus den offiziellen Spieldaten (SDE) von CCP
- Assets, Blaupausen (ME/TE) und Handelsskills des Charakters über EVE SSO und ESI
- Systemkostenindizes und Marktpreise (Jita, Amarr, Dodixie, Rens oder Hek)
- Alles läuft lokal. Die Oberfläche öffnet sich im Browser unter `http://localhost:52173`.

## Download

Fertige Programme für Windows und Mac gibt es unter [Releases](../../releases).

## Selbst bauen

Benötigt Go 1.24 oder neuer.

```bash
# Windows
GOOS=windows GOARCH=amd64 go build -o EVE-Produktionsrechner.exe .
# macOS (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o EVE-Produktionsrechner .
```

Einrichtung (EVE-Anwendung registrieren, Client ID, Scopes): siehe [dist-files/Anleitung.txt](dist-files/Anleitung.txt).

> Hinweis: Der Quellcode hier entspricht Version 1.0.0.
