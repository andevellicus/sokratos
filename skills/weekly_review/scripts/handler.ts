// weekly_review: Queries the database for a 7-day activity summary across
// memories, goals, work items, routines, and personality evolution.

declare function call_tool(name: string, args: Record<string, any>): string;
declare const skill_config: Record<string, any> | undefined;

const cfg = skill_config || {};
const settings = (cfg as any).settings || {};
const ACTIVITY_DAYS = settings.activity_days || 7;
const GOALS_DAYS = settings.goals_days || 30;
const TOP_MEMORIES = settings.top_memories || 5;
const TOP_COMPLETED = settings.top_completed || 5;

interface QueryResult {
  label: string;
  query: string;
}

const queries: QueryResult[] = [
  {
    label: "Memory Activity (" + ACTIVITY_DAYS + " days)",
    query:
      "Count memories by memory_type from the last " + ACTIVITY_DAYS + " days. " +
      "Also return the top " + TOP_MEMORIES + " highest-salience summaries (excluding goal and identity types).",
  },
  {
    label: "Active Goals",
    query:
      "List all memories where memory_type='goal' and superseded_by IS NULL and salience >= 5, " +
      "created in last " + GOALS_DAYS + " days. For each, count work_items from the last " + ACTIVITY_DAYS + " days whose directive " +
      "contains the goal text (ILIKE match), split by status.",
  },
  {
    label: "Work Items (" + ACTIVITY_DAYS + " days)",
    query:
      "Count work_items from the last " + ACTIVITY_DAYS + " days grouped by status. " +
      "Also list the top " + TOP_COMPLETED + " most recently completed directives.",
  },
  {
    label: "Routine Health",
    query:
      "Count work_items where type='routine' from the last " + ACTIVITY_DAYS + " days, grouped by directive. " +
      "Show total runs and completed runs.",
  },
  {
    label: "Personality Evolution",
    query:
      "List personality_traits where updated_at is within the last " + ACTIVITY_DAYS + " days, " +
      "showing category, trait_key, and trait_value.",
  },
];

(function main() {
  const sections: string[] = [];

  for (const q of queries) {
    let result: string;
    try {
      result = call_tool("ask_database", { question: q.query });
    } catch (e) {
      result = "(query failed: " + e + ")";
    }
    sections.push("## " + q.label + "\n" + result);
  }

  return sections.join("\n\n");
})();
