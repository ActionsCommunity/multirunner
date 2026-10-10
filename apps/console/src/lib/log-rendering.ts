export interface PreparedLogLine {
  index: number
  raw: string
  searchable: string
}

export interface PreparedLog {
  lines: PreparedLogLine[]
  characters: number
}

export interface AnsiSegment {
  text: string
  className: string
}

const ansiPattern = new RegExp(
  `${String.fromCharCode(27)}\\[[0-?]*[ -/]*[@-~]`,
  'g',
)
const ansiSGRPattern = new RegExp(
  `${String.fromCharCode(27)}\\[([0-9;]*)m`,
  'g',
)
const ansiColors: Record<number, string> = {
  30: 'ansi-black',
  31: 'ansi-red',
  32: 'ansi-green',
  33: 'ansi-yellow',
  34: 'ansi-blue',
  35: 'ansi-magenta',
  36: 'ansi-cyan',
  37: 'ansi-white',
  90: 'ansi-bright-black',
  91: 'ansi-bright-red',
  92: 'ansi-bright-green',
  93: 'ansi-bright-yellow',
  94: 'ansi-bright-blue',
  95: 'ansi-bright-magenta',
  96: 'ansi-bright-cyan',
  97: 'ansi-bright-white',
}

export function prepareLog(content: string): PreparedLog {
  return {
    characters: content.length,
    lines: content.split(/\r?\n/).map((raw, index) => ({
      index,
      raw,
      searchable: raw.replace(ansiPattern, '').toLowerCase(),
    })),
  }
}

export function filterLogLines(prepared: PreparedLog, query: string) {
  const term = query.trim().toLowerCase()
  return term
    ? prepared.lines.filter((line) => line.searchable.includes(term))
    : prepared.lines
}

export function ansiSegments(value: string): AnsiSegment[] {
  const segments: AnsiSegment[] = []
  let offset = 0
  let bold = false
  let color = ''
  ansiSGRPattern.lastIndex = 0
  for (
    let match = ansiSGRPattern.exec(value);
    match;
    match = ansiSGRPattern.exec(value)
  ) {
    if (match.index > offset) {
      segments.push({
        text: value.slice(offset, match.index),
        className: [bold ? 'ansi-bold' : '', color]
          .filter(Boolean)
          .join(' '),
      })
    }
    const codes = (match[1] || '0').split(';').map(Number)
    for (const code of codes) {
      if (code === 0) {
        bold = false
        color = ''
      } else if (code === 1) {
        bold = true
      } else if (code === 22) {
        bold = false
      } else if (code === 39) {
        color = ''
      } else {
        color = ansiColors[code] ?? color
      }
    }
    offset = match.index + match[0].length
  }
  if (offset < value.length) {
    segments.push({
      text: value.slice(offset),
      className: [bold ? 'ansi-bold' : '', color].filter(Boolean).join(' '),
    })
  }
  return segments
}
