(() => {
  for (const time of document.querySelectorAll("time[data-local]")) {
    const date = new Date(time.dateTime);
    if (!Number.isNaN(date.valueOf())) {
      time.textContent = date.toLocaleString();
      time.title = time.dateTime;
    }
  }
})();
