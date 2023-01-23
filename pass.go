package glitch

import (
	// "fmt"
	// "math"
	"sort"

	"github.com/unitoftime/gl"
)

// TODO - This whole file needs to be rewritten. I'm thinking:
// 1. 2D render pass, (which this file is kind of turning into). But I probably want to focus more on using depth buffer and less on software sorting. And batching static geometry (ie reducing copying)
// 2. 3D render pass (todo - figure out a good way to do that)


type BatchTarget interface {
	Add(*Mesh, Mat4, RGBA, Material)
}

type Target interface {
	// TODO - Should this be differentiated from being a source Vs a target binding. For example, I'm using this now to bind the target that we draw to. But If I want to have another function on frambuffers to use them as image texture inputs, what would that API be called?
	Bind()
}

// https://realtimecollisiondetection.net/blog/?p=86
// Sort by:
// - Front-to-back vs Back-to-front (single bit)
// - Depth bits
// - Material / Uniforms / Textures
// - Sort by: x, y, z, depth?
type drawCommand struct {
	command uint64
	mesh *Mesh
	matrix Mat4
	mask RGBA
	material Material
}

type meshBuffer struct {
	buffer *VertexBuffer
	generation uint32
}
func newMeshBuffer(shader *Shader, mesh *Mesh) meshBuffer {
	if len(mesh.indices) % 3 != 0 {
		panic("Mesh indices must have 3 indices per triangle!")
	}
	numVerts := len(mesh.positions)
	numTris := len(mesh.indices) / 3
	meshBuf := meshBuffer{
		buffer: NewVertexBuffer(shader, numVerts, numTris),
		generation: mesh.generation,
	}
	return meshBuf
}

// type drawCall struct {
// 	buffer *VertexBuffer
// 	model Mat4
// }

// This is essentially a generalized 2D render pass
type RenderPass struct {
	shader *Shader
	texture *Texture
	uniforms map[string]interface{}
	buffer *BufferPool
	commands [][]drawCommand
	currentLayer int8 // TODO - layering code relies on the fact that this is a uint8, when you change, double check every usage of layers.

	DepthTest bool // If set true, enable hardware depth testing. This changes how software sorting works. currently If you change this mid-pass you might get weird behavior.
	SoftwareSort SoftwareSortMode

	minimumCacheSize int // The minimum number of verts a mesh must have before we cache it
	meshCache map[*Mesh]meshBuffer // TODO - this should be some sort of LRU cache

	buffersToDraw []*VertexBuffer
}

type SoftwareSortMode uint8
const (
	SoftwareSortNone SoftwareSortMode = iota
	SoftwareSortX // Sort based on the X position
	SoftwareSortY // Sort based on the Y position
	SoftwareSortZ // Sort based on the Z position
	SoftwareSortCommand // Sort by the computed drawCommand.command
)

// const DefaultLayer uint8 = 127/2
// const DefaultLayer int8 = 0

func NewRenderPass(shader *Shader) *RenderPass {
	defaultBatchSize := 100000
	return &RenderPass{
		shader: shader,
		texture: nil,
		uniforms: make(map[string]interface{}),
		buffer: NewBufferPool(shader, defaultBatchSize),
		commands: make([][]drawCommand, 256), // TODO - hardcoding from sizeof(uint8)
		// currentLayer: DefaultLayer,
		meshCache: make(map[*Mesh]meshBuffer),
		minimumCacheSize: 4*1024, // TODO - 4k is arbitrary
		buffersToDraw: make([]*VertexBuffer, 0),
	}
}

func (r *RenderPass) Clear() {
	// Clear stuff
	r.buffer.Clear()
	// r.commands = r.commands[:0]
	for l := range r.commands {
		r.commands[l] = r.commands[l][:0]
	}

	r.buffersToDraw = r.buffersToDraw[:0]
}

// TODO - I think I could use a linked list of layers and just use an int here
func (r *RenderPass) SetLayer(layer int8) {
	r.currentLayer = layer
}

func (r *RenderPass) Batch() {
	r.SortInSoftware()

	destBuffs := make([]any, len(r.shader.attrFmt))
	for i, attr := range r.shader.attrFmt {
		destBuffs[i] = attr.GetBuffer()
	}

	for l := len(r.commands)-1; l >= 0; l-- { // Reverse order so that layer 0 is drawn last
		for _, c := range r.commands[l] {
			if c.mesh == nil { continue } // Skip nil meshes
			numVerts := len(c.mesh.positions)

			// Try to use a previously batched mesh
			if numVerts > r.minimumCacheSize {
				// Because we are about to use a custom meshBuffer, we need to make sure the next time we do an autobatch we use a new VertexBuffer, So we call this function to foward the autobuffer to the next clean buffer
				r.buffer.gotoNextClean()

				// TODO b/c we are a large mesh, don't do matrix transformation, just apply the model matrix to the buffer in the buffer pool
				meshBuf, ok := r.meshCache[c.mesh]
				if !ok {
					// fmt.Println("MeshCache: Mesh has never been cached!")
					// Create and add the meshBuffer to the cache
					meshBuf = newMeshBuffer(r.shader, c.mesh)
					r.meshCache[c.mesh] = meshBuf

					success := meshBuf.buffer.Reserve(c.material, c.mesh.indices, numVerts, destBuffs)
					if !success {
						panic("Something went wrong")
					}
					r.batchToBuffers(c, destBuffs)
				} else {
					// If here we know we have a valid meshBuf, which either:
					// 1. Has a correct generation - No need to rebuffer it
					// 2. Has an old generation - we need to clear and rebuffer it
					if c.mesh.generation != meshBuf.generation {
						// fmt.Println("MeshCache: Mesh has new generation!")
						meshBuf.buffer.Clear()
						success := meshBuf.buffer.Reserve(c.material, c.mesh.indices, numVerts, destBuffs)
						if !success {
							// If we failed to reserve, then we need to recreate the buffer. Likely the buffer is too small for our current mesh
							meshBuf = newMeshBuffer(r.shader, c.mesh)
							success = meshBuf.buffer.Reserve(c.material, c.mesh.indices, numVerts, destBuffs)
							if !success {
								panic("Something went wrong")
							}
						}
						r.batchToBuffers(c, destBuffs)

						// Update the cache
						meshBuf.generation = c.mesh.generation
						r.meshCache[c.mesh] = meshBuf
					}
				}

				// At this point we have a meshBuf with the mesh data already written to it.
				// We can just append it to our list of things to draw
				r.buffersToDraw = append(r.buffersToDraw, meshBuf.buffer)
			} else {
				// Else we are auto-batching the mesh because the mesh is small
				vertexBuffer := r.buffer.Reserve(c.material, c.mesh.indices, numVerts, destBuffs)
				r.batchToBuffers(c, destBuffs)

				// If the last buffer to draw isn't the currently used vertexBuffer, then we need to add it to the list
				if len(r.buffersToDraw) <= 0 || r.buffersToDraw[len(r.buffersToDraw) - 1] != vertexBuffer {
					r.buffersToDraw = append(r.buffersToDraw, vertexBuffer)
				}
			}
		}
	}
}

// TODO - Mat?
func (r *RenderPass) Draw(target Target) {
	r.Batch()

	// Bind render target
	target.Bind()

	mainthreadCall(func() {
		// 	//https://gamedev.stackexchange.com/questions/134809/how-do-i-sort-with-both-depth-and-y-axis-in-opengl
		if r.DepthTest {
			gl.Enable(gl.DEPTH_TEST)
			gl.DepthFunc(gl.LEQUAL)
		} else {
			gl.Disable(gl.DEPTH_TEST)
		}
	})

	r.shader.Bind()
	for k,v := range r.uniforms {
		ok := r.shader.SetUniform(k, v)
		if !ok {
			panic("Error setting uniform - todo decrease this to log")
		}
	}

	// r.buffer.Draw()
	// openglDraw(r.buffer.buffers)
	openglDraw(r.buffersToDraw)
}

func (r *RenderPass) SetTexture(slot int, texture *Texture) {
	// TODO - use correct texture slot
	r.texture = texture
}

func (r *RenderPass) SetUniform(name string, value interface{}) {
	r.uniforms[name] = value
}

// Option 1: I was thinking that I could add in the Z component on top of the Y component at the very end. but only use the early Y component for the sorting.
// Option 2: I could also just offset the geometry when I create the sprite (or after). Then simply use the transforms like normal. I'd just have to offset the sprite by the height, and then not add the height to the Y transformation
// Option 3: I can batch together these sprites into a single thing that is then rendered
func (r *RenderPass) Add(mesh *Mesh, mat Mat4, mask RGBA, material Material) {
	if r.DepthTest {
		// If we are doing depth testing, then use the r.CurrentLayer field to determine the depth (normalizing from (0 to 1). Notably the standard ortho cam is (-1, 1) which this range fits into but is easier to normalize to // TODO - make that depth range tweakable?
		// TODO - hardcoded because layer is a uint8. You probably want to make layer an int and then just set depth based on that
		// depth := 1 - (float32(r.currentLayer) / float32(math.MaxUint8))
		// // fmt.Println("Old/New: ", mat[i4_3_2], depth)
		// mat[i4_3_2] = depth // Set Z translation to the depth

		// fmt.Println("Apply: ", mat.Apply(Vec3{0, 0, 0}))

		// mat[i4_3_2] = mat[i4_3_1] // Set Z translation to the y point

		// Add the layer to the depth
		mat[i4_3_2] -= float64(r.currentLayer)
		// fmt.Println("Depth: ", mat[i4_3_2])

		r.commands[0] = append(r.commands[0], drawCommand{
			0, mesh, mat, mask, material,
		})
	} else {
		r.commands[r.currentLayer] = append(r.commands[r.currentLayer], drawCommand{
			0, mesh, mat, mask, material,
		})
	}
}

func (r *RenderPass) SortInSoftware() {
	if r.DepthTest {
		// TODO - do special sort function for depth test code:
		// 1. Fully Opaque or fully transparent groups of meshes: Don't sort inside that group
		// 2. Partially transparent groups of meshes: sort inside that group
		return
	}
	if r.SoftwareSort == SoftwareSortNone { return } // Skip if sorting disabled

	if r.SoftwareSort == SoftwareSortX {
		for c := range r.commands {
			sort.Slice(r.commands[c], func(i, j int) bool {
				return r.commands[c][i].matrix[i4_3_0] > r.commands[c][j].matrix[i4_3_0] // sort by x
			})
		}
	} else if r.SoftwareSort == SoftwareSortY {
		for c := range r.commands {
			sort.Slice(r.commands[c], func(i, j int) bool {
				return r.commands[c][i].matrix[i4_3_1] > r.commands[c][j].matrix[i4_3_1] // Sort by y
			})
		}
	} else if r.SoftwareSort == SoftwareSortZ {
		for c := range r.commands {
			sort.Slice(r.commands[c], func(i, j int) bool {
				return r.commands[c][i].matrix[i4_3_2] > r.commands[c][j].matrix[i4_3_2] // Sort by z
			})
		}
	} else if r.SoftwareSort == SoftwareSortCommand {
		for c := range r.commands {
			sort.Slice(r.commands[c], func(i, j int) bool {
				return r.commands[c][i].command > r.commands[c][j].command // Sort by command
			})
		}
	}
}

func (r *RenderPass) batchToBuffers(c drawCommand, destBuffs []interface{}) {
	mat32 := c.matrix.gl()

	// Append all mesh buffers to shader buffers
	for bufIdx, attr := range r.shader.attrFmt {
		// TODO - I'm not sure of a good way to break up this switch statement
		switch attr.Swizzle {
			// Positions
		case PositionXY:
			posBuf := *(destBuffs[bufIdx]).(*[]glVec2)
			if c.matrix == Mat4Ident {
				// If matrix is identity, don't transform anything
				for i := range c.mesh.positions {
					posBuf[i] = *(*glVec2)(c.mesh.positions[i][:2])
				}
			} else {
				for i := range c.mesh.positions {
					vec := mat32.Apply(c.mesh.positions[i])
					posBuf[i] = *(*glVec2)(vec[:2])
				}
			}

		case PositionXYZ:
			posBuf := *(destBuffs[bufIdx]).(*[]glVec3)
			if c.matrix == Mat4Ident {
				// If matrix is identity, don't transform anything
				for i := range c.mesh.positions {
					posBuf[i] = c.mesh.positions[i]
				}
			} else {
				for i := range c.mesh.positions {
					vec := mat32.Apply(c.mesh.positions[i])
					posBuf[i] = vec
				}
			}

			// Normals
			// TODO - Renormalize if batching
			// case NormalXY:
			// 	normBuf := *(destBuffs[bufIdx]).(*[]Vec2)
			// 	for i := range c.mesh.normals {
			// 		vec := c.mesh.normals[i]
			// 		normBuf[i] = *(*Vec2)(vec[:2])
			// 	}

		case NormalXYZ:
			renormalizeMat := c.matrix.Inv().Transpose().gl()
			normBuf := *(destBuffs[bufIdx]).(*[]glVec3)
			for i := range c.mesh.normals {
				vec := renormalizeMat.Apply(c.mesh.normals[i])
				normBuf[i] = vec
			}

			// Colors
		case ColorR:
			colBuf := *(destBuffs[bufIdx]).(*[]float32)
			for i := range c.mesh.colors {
				colBuf[i] = c.mesh.colors[i][0] * float32(c.mask.R)
			}
		case ColorRG:
			colBuf := *(destBuffs[bufIdx]).(*[]glVec2)
			for i := range c.mesh.colors {
				colBuf[i] = glVec2{
					c.mesh.colors[i][0] * float32(c.mask.R),
					c.mesh.colors[i][1] * float32(c.mask.G),
				}
			}
		case ColorRGB:
			colBuf := *(destBuffs[bufIdx]).(*[]glVec3)
			for i := range c.mesh.colors {
				colBuf[i] = glVec3{
					c.mesh.colors[i][0] * float32(c.mask.R),
					c.mesh.colors[i][1] * float32(c.mask.G),
					c.mesh.colors[i][2] * float32(c.mask.B),
				}
			}
		case ColorRGBA:
			colBuf := *(destBuffs[bufIdx]).(*[]glVec4)
			for i := range c.mesh.colors {
				colBuf[i] = glVec4{
					c.mesh.colors[i][0] * float32(c.mask.R),
					c.mesh.colors[i][1] * float32(c.mask.G),
					c.mesh.colors[i][2] * float32(c.mask.B),
					c.mesh.colors[i][3] * float32(c.mask.A),
				}
			}

		case TexCoordXY:
			texBuf := *(destBuffs[bufIdx]).(*[]glVec2)
			for i := range c.mesh.texCoords {
				texBuf[i] = c.mesh.texCoords[i]
			}
		default:
			panic("Unsupported")
		}
	}

	//================================================================================
	// TODO The hardcoding is a bit slower. Keeping it around in case I want to do some performance analysis
	// Notes: Ran gophermark with 1000000 gophers.
	// - Hardcoded: ~ 120 to 125 ms range
	// - Switch Statement: ~ 125 to 130 ms range
	// - Switch Statement (with shader changed to use vec2s for position): ~ 122 to 127 ms range
	// work and append
	// 	posBuf := *(destBuffs[0]).(*[]Vec3)
	// 	for i := range c.mesh.positions {
	// 		vec := c.matrix.Apply(c.mesh.positions[i])
	// 		posBuf[i] = vec
	// 	}

	// 	colBuf := *(destBuffs[1]).(*[]Vec4)
	// 	for i := range c.mesh.colors {
	// 		colBuf[i] = Vec4{
	// 			c.mesh.colors[i][0] * c.mask.R,
	// 			c.mesh.colors[i][1] * c.mask.G,
	// 			c.mesh.colors[i][2] * c.mask.B,
	// 			c.mesh.colors[i][3] * c.mask.A,
	// 		}
	// 	}

	// 	texBuf := *(destBuffs[2]).(*[]Vec2)
	// 	for i := range c.mesh.texCoords {
	// 		texBuf[i] = c.mesh.texCoords[i]
	// 	}
	//================================================================================
}
