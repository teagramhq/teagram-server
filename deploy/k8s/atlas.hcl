env "k8s" {
  url = getenv("TG_POSTGRES_DSN")
  migration {
    dir = "file:///migrations"
  }
}
