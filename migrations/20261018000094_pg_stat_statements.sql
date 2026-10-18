CREATE EXTENSION pg_stat_statements;

REVOKE ALL ON TABLE public.pg_stat_statements, public.pg_stat_statements_info FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION public.pg_stat_statements(boolean), public.pg_stat_statements_info() FROM PUBLIC;

GRANT SELECT ON TABLE public.pg_stat_statements, public.pg_stat_statements_info TO pg_read_all_stats;
GRANT EXECUTE ON FUNCTION public.pg_stat_statements(boolean), public.pg_stat_statements_info() TO pg_read_all_stats;
