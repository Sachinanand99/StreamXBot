package models

// PartyJoinRequest is the identity submitted when creating or joining a party.
// Field names intentionally match the StreamX listen-together client wire format.
type PartyJoinRequest struct {
	UserID          string  `json:"userId"`
	DeviceID        string  `json:"deviceId"`
	DisplayName     string  `json:"displayName"`
	AvatarURL       *string `json:"avatarUrl,omitempty"`
	MaxMembers      *int    `json:"maxMembers,omitempty"`
	AutoplayEnabled *bool   `json:"autoplayEnabled,omitempty"`
}

// PartyAuthResponse is returned by create and join.
type PartyAuthResponse struct {
	Code  string         `json:"code"`
	Token string         `json:"token"`
	You   map[string]any `json:"you"`
	Party map[string]any `json:"party"`
}

// PartyPreviewMember is a public member summary used by the preview route.
type PartyPreviewMember struct {
	DisplayName string  `json:"displayName"`
	AvatarURL   *string `json:"avatarUrl"`
	IsHost      bool    `json:"isHost"`
}

// PartyPreview is the unauthenticated party preview payload.
type PartyPreview struct {
	Code        string               `json:"code"`
	HostName    string               `json:"hostName"`
	MemberCount int                  `json:"memberCount"`
	MaxMembers  int                  `json:"maxMembers"`
	IsFull      bool                 `json:"isFull"`
	Members     []PartyPreviewMember `json:"members"`
}

// PresenceRequest is the open-app heartbeat payload.
type PresenceRequest struct {
	ID       string `json:"id"`
	Platform string `json:"platform"`
	Open     *bool  `json:"open"`
}

// PartyLiveStats is the public open-app count payload.
type PartyLiveStats struct {
	Online    int   `json:"online"`
	Android   int   `json:"android"`
	PC        int   `json:"pc"`
	UpdatedAt int64 `json:"updatedAt"`
}
