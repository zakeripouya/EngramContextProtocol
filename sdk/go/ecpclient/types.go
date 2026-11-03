package ecpclient

// Types mirror spec/schema.ts (the normative ECP schema). Fields are optional
// where the schema marks them optional.

// Provenance says who wrote a memory and how.
type Provenance struct {
	Agent     string `json:"agent"`
	Client    string `json:"client"`
	Service   string `json:"service,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	Author    string `json:"author"`
	At        string `json:"at"`
}

// Decision is a choice the user made.
type Decision struct {
	Situation string   `json:"situation"`
	Options   []string `json:"options,omitempty"`
	Choice    string   `json:"choice"`
	Rationale string   `json:"rationale,omitempty"`
}

// Memory is one node of the memory graph.
type Memory struct {
	ID              string     `json:"id"`
	Kind            string     `json:"kind"`
	Content         string     `json:"content"`
	Tags            []string   `json:"tags,omitempty"`
	Scope           string     `json:"scope"`
	Namespace       string     `json:"namespace"`
	Strength        float64    `json:"strength"`
	Confidence      *float64   `json:"confidence,omitempty"`
	Decision        *Decision  `json:"decision,omitempty"`
	Provenance      Provenance `json:"provenance"`
	CreatedAt       string     `json:"createdAt"`
	ValidFrom       string     `json:"validFrom,omitempty"`
	ValidTo         string     `json:"validTo,omitempty"`
	LastActivatedAt string     `json:"lastActivatedAt,omitempty"`
}

// Edge is a weighted link between memories.
type Edge struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Kind   string  `json:"kind"`
	Weight float64 `json:"weight"`
}

// ScoredMemory is a recall result.
type ScoredMemory struct {
	Memory     Memory   `json:"memory"`
	Score      float64  `json:"score"`
	Confidence float64  `json:"confidence,omitempty"`
	Path       []string `json:"path,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

// ObserveParams writes a memory.
type ObserveParams struct {
	Kind        string    `json:"kind"`
	Content     string    `json:"content"`
	Decision    *Decision `json:"decision,omitempty"`
	Tags        []string  `json:"tags,omitempty"`
	Scope       string    `json:"scope,omitempty"`
	Namespace   string    `json:"namespace,omitempty"`
	Service     string    `json:"service,omitempty"`
	SessionID   string    `json:"sessionId,omitempty"`
	Author      string    `json:"author,omitempty"`
	ValidFrom   string    `json:"validFrom,omitempty"`
	Supersedes  string    `json:"supersedes,omitempty"`
	Contradicts []string  `json:"contradicts,omitempty"`
}

// ObserveResult is the stored memory's id.
type ObserveResult struct {
	ID       string `json:"id"`
	Linked   int    `json:"linked"`
	Embedded bool   `json:"embedded,omitempty"`
}

// RecallParams queries memory.
type RecallParams struct {
	Query        string   `json:"query"`
	K            int      `json:"k,omitempty"`
	Kinds        []string `json:"kinds,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	Namespaces   []string `json:"namespaces,omitempty"`
	Spread       *int     `json:"spread,omitempty"`
	AsOf         string   `json:"asOf,omitempty"`
	IncludePaths bool     `json:"includePaths,omitempty"`
}

// PathResult is a chain of associations.
type PathResult struct {
	Path        []string `json:"path"`
	WeakestLink *Edge    `json:"weakestLink,omitempty"`
}

// AttendParams asks what matters now.
type AttendParams struct {
	Context    string   `json:"context"`
	Goals      []string `json:"goals,omitempty"`
	K          int      `json:"k,omitempty"`
	Mode       string   `json:"mode,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`
}

// Conflict is an unresolved disagreement.
type Conflict struct {
	A       string `json:"a"`
	B       string `json:"b"`
	Summary string `json:"summary"`
}

// PendingNotification is a stored push.
type PendingNotification struct {
	ID             int64    `json:"id"`
	SubscriptionID string   `json:"subscriptionId"`
	EventID        string   `json:"eventId"`
	Reason         string   `json:"reason"`
	Salience       float64  `json:"salience"`
	Memories       []Memory `json:"memories"`
	CreatedAt      string   `json:"createdAt"`
}

// AttendResult is what matters now.
type AttendResult struct {
	Memories  []ScoredMemory        `json:"memories"`
	Conflicts []Conflict            `json:"conflicts"`
	Pending   []PendingNotification `json:"pending,omitempty"`
}

// FeedbackParams reports how recalled memories worked out.
type FeedbackParams struct {
	MemoryIDs  []string  `json:"memoryIds"`
	Outcome    string    `json:"outcome"`
	Objective  string    `json:"objective,omitempty"`
	Correction *Decision `json:"correction,omitempty"`
	Query      string    `json:"query,omitempty"`
}

// FeedbackResult says what changed.
type FeedbackResult struct {
	Updated      int    `json:"updated"`
	Direction    string `json:"direction"`
	CorrectionID string `json:"correctionId,omitempty"`
}

// SubscribeParams registers interest in a context.
type SubscribeParams struct {
	Context    string   `json:"context"`
	Tags       []string `json:"tags,omitempty"`
	Threshold  float64  `json:"threshold,omitempty"`
	Namespaces []string `json:"namespaces,omitempty"`
}

// Environment is what a task needs, or what a resuming client has.
type Environment struct {
	Repo  string   `json:"repo,omitempty"`
	Tools []string `json:"tools,omitempty"`
}

// Checkpoint is the state of a task in progress.
type Checkpoint struct {
	SessionID     string       `json:"sessionId,omitempty"`
	Goal          string       `json:"goal,omitempty"`
	Plan          []string     `json:"plan,omitempty"`
	Done          []string     `json:"done,omitempty"`
	Next          string       `json:"next,omitempty"`
	OpenQuestions []string     `json:"openQuestions,omitempty"`
	Files         []string     `json:"files,omitempty"`
	Environment   *Environment `json:"environment,omitempty"`
	Namespace     string       `json:"namespace,omitempty"`
	Status        string       `json:"status,omitempty"`
	Result        string       `json:"result,omitempty"`
	ParentID      string       `json:"parentId,omitempty"`
	DelegatedTo   string       `json:"delegatedTo,omitempty"`
	CreatedBy     string       `json:"createdBy,omitempty"`
	LastClient    string       `json:"lastClient,omitempty"`
	CreatedAt     string       `json:"createdAt,omitempty"`
	UpdatedAt     string       `json:"updatedAt,omitempty"`
}

// ResumeResult is a checkpoint plus what the resuming client lacks.
type ResumeResult struct {
	Checkpoint Checkpoint `json:"checkpoint"`
	Missing    []string   `json:"missing"`
	Subtasks   []struct {
		SessionID   string `json:"sessionId"`
		Goal        string `json:"goal"`
		Status      string `json:"status"`
		DelegatedTo string `json:"delegatedTo,omitempty"`
		Result      string `json:"result,omitempty"`
	} `json:"subtasks,omitempty"`
	Memories []Memory `json:"memories,omitempty"`
}

// InitializeResult is the server's answer to initialize.
type InitializeResult struct {
	ProtocolVersion string                    `json:"protocolVersion"`
	Capabilities    map[string]map[string]any `json:"capabilities"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

// Has reports whether the server declared a capability.
func (r InitializeResult) Has(capability string) bool { _, ok := r.Capabilities[capability]; return ok }
