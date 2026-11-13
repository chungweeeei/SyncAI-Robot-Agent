package backend

import (
	"context"
	"net/url"
)

// VertexMove is the type of an ordinary stop; the other one is "CHARGE" (a charger).
const VertexMove = "MOVE"

// Vertex is a named point on a map, one entry of GET /api/v1/maps/{name}/vertices.
// Names are unique within a map.
type Vertex struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Type  string  `json:"type"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Theta float64 `json:"theta"` // degrees
	Z     float64 `json:"z"`     // height of the ground the vertex stands on; tells storeys apart
}

// GetVertices lists every vertex of a map.
func (c *Client) GetVertices(ctx context.Context, mapName string) ([]Vertex, error) {
	var out []Vertex
	err := c.getJSON(ctx, "/api/v1/maps/"+url.PathEscape(mapName)+"/vertices", &out)
	return out, err
}
