pub mod util;

pub struct Widget {
    pub name: String,
}

impl Widget {
    pub fn new(name: &str) -> Widget {
        Widget { name: name.to_string() }
    }
}

pub fn build() -> Widget {
    Widget::new("default")
}
