package viz

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/store"
)

type nodeData struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	QualifiedName string `json:"qualifiedName"`
	Kind          string `json:"kind"`
	File          string `json:"file"`
	StartLine     int    `json:"startLine"`
	EndLine       int    `json:"endLine"`
}

type edgeData struct {
	Source int64  `json:"source"`
	Target int64  `json:"target"`
	Kind   string `json:"kind"`
}

// GenerateHTML writes a self-contained HTML file with an interactive D3.js
// force-directed graph visualization of the given symbols and edges.
func GenerateHTML(w io.Writer, symbols []graph.Symbol, edges []store.ExportEdge) error {
	// Build node list.
	nodes := make([]nodeData, len(symbols))
	nodeSet := make(map[int64]bool, len(symbols))
	for i, s := range symbols {
		nodes[i] = nodeData{
			ID:            s.ID,
			Name:          s.Name,
			QualifiedName: s.QualifiedName,
			Kind:          s.Kind,
			File:          s.FilePath,
			StartLine:     s.Range.StartLine,
			EndLine:       s.Range.EndLine,
		}
		nodeSet[s.ID] = true
	}

	// Build edge list, keeping only edges where both endpoints exist.
	var links []edgeData
	for _, e := range edges {
		if e.DstSymbolID == nil {
			continue
		}
		if !nodeSet[e.SrcSymbolID] || !nodeSet[*e.DstSymbolID] {
			continue
		}
		links = append(links, edgeData{
			Source: e.SrcSymbolID,
			Target: *e.DstSymbolID,
			Kind:   e.Kind,
		})
	}

	nodesJSON, err := json.Marshal(nodes)
	if err != nil {
		return fmt.Errorf("marshal nodes: %w", err)
	}
	edgesJSON, err := json.Marshal(links)
	if err != nil {
		return fmt.Errorf("marshal edges: %w", err)
	}

	data := map[string]template.JS{
		"NodesJSON": template.JS(nodesJSON),
		"EdgesJSON": template.JS(edgesJSON),
	}

	return htmlTemplate.Execute(w, data)
}

var htmlTemplate = template.Must(template.New("graph").Parse(htmlTmpl))

const htmlTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>codegraph visualization</title>
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body {
  background: #1a1a2e;
  color: #e0e0e0;
  font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
  overflow: hidden;
}
#controls {
  position: fixed;
  top: 12px;
  left: 12px;
  z-index: 10;
  display: flex;
  gap: 8px;
  align-items: center;
}
#search {
  padding: 6px 12px;
  border: 1px solid #444;
  border-radius: 4px;
  background: #16213e;
  color: #e0e0e0;
  font-size: 14px;
  width: 260px;
}
#search::placeholder { color: #888; }
#info {
  position: fixed;
  top: 12px;
  right: 12px;
  z-index: 10;
  font-size: 12px;
  color: #888;
}
#tooltip {
  position: fixed;
  display: none;
  padding: 8px 12px;
  background: #0f3460;
  border: 1px solid #555;
  border-radius: 4px;
  font-size: 12px;
  pointer-events: none;
  z-index: 20;
  max-width: 400px;
  line-height: 1.5;
}
svg { display: block; }
.link {
  stroke: #555;
  stroke-opacity: 0.5;
  fill: none;
}
.link.highlighted {
  stroke: #e94560;
  stroke-opacity: 1;
  stroke-width: 2px;
}
.node circle {
  stroke: #222;
  stroke-width: 1.5px;
  cursor: pointer;
}
.node text {
  font-size: 10px;
  fill: #ccc;
  pointer-events: none;
  text-anchor: middle;
  dominant-baseline: central;
  dy: -12px;
}
.node.dimmed circle { opacity: 0.15; }
.node.dimmed text { opacity: 0.15; }
.link.dimmed { stroke-opacity: 0.05; }
.node.search-match circle {
  stroke: #f0e68c;
  stroke-width: 3px;
}
</style>
</head>
<body>
<div id="controls">
  <input id="search" type="text" placeholder="Search symbols..." autocomplete="off">
</div>
<div id="info">scroll to zoom &middot; drag to pan &middot; click node to highlight</div>
<div id="tooltip"></div>
<svg id="graph"></svg>
<script src="https://d3js.org/d3.v7.min.js"></script>
<script>
(function() {
  const nodes = {{.NodesJSON}};
  const links = {{.EdgesJSON}};

  const kindColor = {
    "function": "#4a90d9",
    "method":   "#e07020",
    "type":     "#50b050",
    "class":    "#50b050",
    "struct":   "#50b050",
    "interface":"#88c088",
    "variable": "#c080d0",
    "constant": "#d0a060",
    "field":    "#70b0b0",
    "property": "#70b0b0",
    "module":   "#d06070",
    "package":  "#d06070",
  };
  function color(kind) {
    return kindColor[(kind || "").toLowerCase()] || "#999";
  }

  const width = window.innerWidth;
  const height = window.innerHeight;

  const svg = d3.select("#graph")
    .attr("width", width)
    .attr("height", height);

  const defs = svg.append("defs");
  defs.append("marker")
    .attr("id", "arrow")
    .attr("viewBox", "0 -4 8 8")
    .attr("refX", 16)
    .attr("refY", 0)
    .attr("markerWidth", 6)
    .attr("markerHeight", 6)
    .attr("orient", "auto")
    .append("path")
    .attr("d", "M0,-4L8,0L0,4")
    .attr("fill", "#555");

  const g = svg.append("g");

  const zoom = d3.zoom()
    .scaleExtent([0.05, 8])
    .on("zoom", (event) => g.attr("transform", event.transform));
  svg.call(zoom);

  // Build node map for link resolution.
  const nodeById = new Map(nodes.map(n => [n.id, n]));

  // Convert links to use object references.
  const validLinks = links.filter(l => nodeById.has(l.source) && nodeById.has(l.target));

  const simulation = d3.forceSimulation(nodes)
    .force("link", d3.forceLink(validLinks).id(d => d.id).distance(80))
    .force("charge", d3.forceManyBody().strength(-200))
    .force("center", d3.forceCenter(width / 2, height / 2))
    .force("collide", d3.forceCollide(20));

  const link = g.append("g")
    .selectAll("path")
    .data(validLinks)
    .join("path")
    .attr("class", "link")
    .attr("marker-end", "url(#arrow)");

  const node = g.append("g")
    .selectAll("g")
    .data(nodes)
    .join("g")
    .attr("class", "node")
    .call(d3.drag()
      .on("start", dragStarted)
      .on("drag", dragged)
      .on("end", dragEnded));

  node.append("circle")
    .attr("r", 7)
    .attr("fill", d => color(d.kind));

  node.append("text")
    .text(d => d.name);

  const tooltip = d3.select("#tooltip");

  node.on("mouseover", (event, d) => {
    tooltip.style("display", "block")
      .html(
        "<strong>" + d.name + "</strong><br>" +
        "Kind: " + d.kind + "<br>" +
        "Qualified: " + d.qualifiedName + "<br>" +
        (d.file ? "File: " + d.file + "<br>" : "") +
        "Lines: " + d.startLine + " - " + d.endLine
      );
  }).on("mousemove", (event) => {
    tooltip.style("left", (event.clientX + 14) + "px")
      .style("top", (event.clientY + 14) + "px");
  }).on("mouseout", () => {
    tooltip.style("display", "none");
  });

  // Click to highlight connected nodes.
  let selectedNode = null;
  node.on("click", (event, d) => {
    event.stopPropagation();
    if (selectedNode === d.id) {
      selectedNode = null;
      node.classed("dimmed", false);
      link.classed("dimmed", false).classed("highlighted", false);
      return;
    }
    selectedNode = d.id;
    const connected = new Set([d.id]);
    validLinks.forEach(l => {
      const sid = l.source.id !== undefined ? l.source.id : l.source;
      const tid = l.target.id !== undefined ? l.target.id : l.target;
      if (sid === d.id) connected.add(tid);
      if (tid === d.id) connected.add(sid);
    });
    node.classed("dimmed", n => !connected.has(n.id));
    link.classed("dimmed", l => {
      const sid = l.source.id !== undefined ? l.source.id : l.source;
      const tid = l.target.id !== undefined ? l.target.id : l.target;
      return sid !== d.id && tid !== d.id;
    });
    link.classed("highlighted", l => {
      const sid = l.source.id !== undefined ? l.source.id : l.source;
      const tid = l.target.id !== undefined ? l.target.id : l.target;
      return sid === d.id || tid === d.id;
    });
  });

  svg.on("click", () => {
    selectedNode = null;
    node.classed("dimmed", false);
    link.classed("dimmed", false).classed("highlighted", false);
  });

  // Search.
  const searchInput = d3.select("#search");
  searchInput.on("input", function() {
    const q = this.value.trim().toLowerCase();
    if (q === "") {
      node.classed("search-match", false).classed("dimmed", false);
      link.classed("dimmed", false);
      return;
    }
    node.classed("search-match", d => d.name.toLowerCase().includes(q));
    node.classed("dimmed", d => !d.name.toLowerCase().includes(q));
    link.classed("dimmed", true);
  });

  simulation.on("tick", () => {
    link.attr("d", d => {
      return "M" + d.source.x + "," + d.source.y +
             "L" + d.target.x + "," + d.target.y;
    });
    node.attr("transform", d => "translate(" + d.x + "," + d.y + ")");
  });

  function dragStarted(event, d) {
    if (!event.active) simulation.alphaTarget(0.3).restart();
    d.fx = d.x;
    d.fy = d.y;
  }
  function dragged(event, d) {
    d.fx = event.x;
    d.fy = event.y;
  }
  function dragEnded(event, d) {
    if (!event.active) simulation.alphaTarget(0);
    d.fx = null;
    d.fy = null;
  }
})();
</script>
</body>
</html>
`
